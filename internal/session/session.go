// Package session runs one sandboxed claude session: it launches the docker
// container, tees the stream-json transcript to disk, narrates progress to the
// console, and enforces a wall-clock cap. It is the shared container-launch +
// transcript-tee plumbing the three cmd/ tools (implementation, review,
// retrospective) build on (DESIGN.md "Build order").
package session

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/stream"
)

// Logger is the narration + transcript sink a session writes through
// (*runlog.Logger satisfies it).
type Logger interface {
	Event(msg string)
	TeeLine(file, raw string)
}

// Options parameterises one Run.
type Options struct {
	// ContainerName is the docker --name, so the harness can `docker kill` it on timeout.
	ContainerName string
	// TranscriptFile is the filename (under the ticket's log dir) the stream-json
	// transcript is teed to.
	TranscriptFile string
	// Timeout is the wall-clock cap; on expiry the container is killed.
	Timeout time.Duration
	// Verbose echoes the raw agent stream to the console instead of the concise narration.
	Verbose bool
	// Log is the narration + transcript sink.
	Log Logger
}

// stderrTailLines is how many trailing non-empty stderr lines to keep so a launch
// failure can be explained on the console (docker prints the real cause then a
// generic "See '… --help'." trailer, so one line isn't enough).
const stderrTailLines = 10

// Outcome is the result of a finished session: the container's exit code, plus
// whether the stream ended on a terminal usage-policy refusal (BEH-389) — a
// known intermittent false-positive the caller may choose to retry rather than
// treat as a real failure.
type Outcome struct {
	// ExitCode is the container's exit code (1 on any launch failure).
	ExitCode int
	// UsagePolicyRefusal is true iff the stream carried the terminal usage-policy
	// refusal result event. The diff survives on disk (ADR-0002 real-path mount),
	// so the caller can retry the session rather than discard the run.
	UsagePolicyRefusal bool
}

// Run launches the sandboxed session described by dockerArgs (everything after
// `docker`), teeing the transcript and narrating progress. It returns the
// container's exit code (1 on any launch failure) plus whether a usage-policy
// refusal was seen. On a docker-cannot-start exit (125) it surfaces docker's own
// reason on the console — otherwise the operator sees a bare exit code (BEH-316).
func Run(dockerArgs []string, opts Options) Outcome {
	cmd := exec.Command("docker", dockerArgs...) // allow-unbounded-exec: main docker run, bounded by the timer-based kill below
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		opts.Log.Event("session ✗ failed to pipe docker stdout: " + err.Error())
		return Outcome{ExitCode: 1}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		opts.Log.Event("session ✗ failed to pipe docker stderr: " + err.Error())
		return Outcome{ExitCode: 1}
	}

	if err := cmd.Start(); err != nil {
		opts.Log.Event("session ✗ failed to launch docker: " + err.Error())
		return Outcome{ExitCode: 1}
	}

	// Hard wall-clock cap: if the session overruns, kill the container out from under it.
	timer := time.AfterFunc(opts.Timeout, func() {
		opts.Log.Event("session ✗ wall-clock cap hit — killing " + opts.ContainerName)
		_ = exec.Command("docker", "kill", opts.ContainerName).Run() // allow-unbounded-exec: docker kill from the timeout handler itself
	})
	defer timer.Stop()

	var (
		wg              sync.WaitGroup
		tail            []string
		usagePolicyDeny bool
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		usagePolicyDeny = pumpStdout(stdout, opts.TranscriptFile, opts.Verbose, opts.Log, os.Stdout)
	}()
	go func() {
		defer wg.Done()
		tail = pumpStderr(stderr, opts.TranscriptFile, opts.Log, stderrTailLines)
	}()
	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Exit 125 means docker couldn't start the container at all (daemon down,
	// image missing, bad flag). The reason is teed only to the transcript, so
	// echo the real cause to the console (BEH-316).
	if exitCode == sandbox.ExitCannotStart {
		hint := sandbox.DockerErrorReason(strings.Join(tail, "\n"))
		if hint == "" {
			hint = "see transcript for docker's error"
		}
		opts.Log.Event("session ✗ docker could not start the container (exit 125): " + hint)
	}

	return Outcome{ExitCode: exitCode, UsagePolicyRefusal: usagePolicyDeny}
}

// pumpStdout scans claude's stream-json stdout: it tees every line raw to the
// transcript, then either echoes the raw line (verbose) or surfaces the concise
// narration for narratable lines. A malformed line is teed but skipped for
// narration so one bad chunk can't kill the run. It returns whether the stream
// carried the terminal usage-policy refusal (BEH-389), checked on every line
// regardless of --verbose so a retryable refusal is never missed.
func pumpStdout(r io.Reader, transcriptFile string, verbose bool, log Logger, echo io.Writer) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	usagePolicyRefusal := false
	for sc.Scan() {
		line := sc.Text()
		log.TeeLine(transcriptFile, line)
		if stream.IsUsagePolicyRefusal(line) {
			usagePolicyRefusal = true
		}
		if verbose {
			fmt.Fprintln(echo, line)
			continue
		}
		if msg, ok := stream.Narrate(line); ok {
			log.Event(msg)
		}
	}
	return usagePolicyRefusal
}

// pumpStderr tees every stderr line (forensic only) and returns a bounded tail of
// the last non-empty lines, so a launch failure can be explained on the console.
func pumpStderr(r io.Reader, transcriptFile string, log Logger, tailLines int) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var tail []string
	for sc.Scan() {
		line := sc.Text()
		log.TeeLine(transcriptFile, line)
		if t := strings.TrimSpace(line); t != "" {
			tail = append(tail, t)
			if len(tail) > tailLines {
				tail = tail[len(tail)-tailLines:]
			}
		}
	}
	return tail
}
