// Command watch is the read-only loop viewer (ADR-0005): it tails the global
// structured event stream (agent-harness/logs/loop.jsonl) the daemon and the
// single-shot pipeline write, and prints each event as one plain line surfacing the
// current ticket and stage. No ANSI, no animation — this is deliberately the plain
// fallback the later animated dashboard degrades to on a non-TTY, so piping or
// redirecting it stays clean.
//
// It NEVER controls the loop — `touch agent-harness/STOP` remains the only control
// path, and quitting the viewer (Ctrl-C) does not touch the daemon. It holds no
// credentials: it only reads a log file, so it does not load the harness config.
//
// Path resolution, in order: a positional argument; else $HERD_PATH's
// agent-harness/logs/loop.jsonl; else logs/loop.jsonl relative to the cwd (the
// harness dir, where `make watch` runs). When the file is absent it prints a clear
// "loop not running" line and keeps polling, so starting the daemon later just
// works without restarting the viewer.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/viewer"
)

// pollInterval is how often the viewer re-reads the stream for new lines. It is a
// plain poll (no inotify/fsevents — stdlib only); sub-second so the live view feels
// responsive without busy-spinning.
const pollInterval = 500 * time.Millisecond

func main() {
	path := resolvePath(os.Args[1:])
	fmt.Printf("watching %s — Ctrl-C to quit (this does not stop the loop)\n", path)

	tl := viewer.NewTailer(path)
	// wasRunning tracks the file's presence so the not-running notice prints once on
	// each transition to absent, not on every poll.
	running := true
	for {
		isRunning, err := tl.Poll(os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		if !isRunning && running {
			fmt.Println(viewer.NotRunningMessage)
		}
		running = isRunning
		time.Sleep(pollInterval)
	}
}

// resolvePath picks the stream file to tail (see the package doc for the order).
func resolvePath(argv []string) string {
	if len(argv) > 0 && argv[0] != "" {
		return argv[0]
	}
	if herd := os.Getenv("HERD_PATH"); herd != "" {
		return loopstream.PathUnder(filepath.Join(herd, "agent-harness", "logs"))
	}
	return loopstream.PathUnder("logs")
}
