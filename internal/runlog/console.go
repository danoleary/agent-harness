package runlog

import (
	"fmt"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// Console is the loop-level narration sink used by cmd/loop and cmd/pipeline for
// events that happen BEFORE (or outside) a ticket-keyed Logger — ticket selection,
// idle/backoff, stop, the circuit breaker. It matches the runlog console format
// (a UTC-timestamped line) and, when a global stream is wired, mirrors Structured
// events into the same loop.jsonl the per-ticket loggers feed (ADR-0005). It holds
// no per-ticket state, so structured records must name their own ticket.
type Console struct {
	stream *loopstream.Stream
}

// NewConsole returns a Console narrating to stdout, additionally appending
// Structured events to stream when non-nil.
func NewConsole(stream *loopstream.Stream) *Console { return &Console{stream: stream} }

// Event narrates one line to the console. Like Logger.Event it does not touch the
// global stream — only Structured events (the closed-enum kinds) feed loop.jsonl.
func (c *Console) Event(message string) { c.narrate(message) }

// Structured narrates the line and, when a stream is wired, appends the structured
// record (kind/ticket/stage) to the global loop.jsonl, sharing the console line's
// timestamp.
func (c *Console) Structured(r loopstream.Record) {
	ts := c.narrate(r.Message)
	if c.stream == nil {
		return
	}
	r.TS = ts
	_ = c.stream.Append(r)
}

func (c *Console) narrate(message string) string {
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("%s  %s\n", ts, message)
	return ts
}
