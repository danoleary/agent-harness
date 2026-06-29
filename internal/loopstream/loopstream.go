// Package loopstream is the daemon→viewer contract (ADR-0005): a global,
// structured event stream the harness writes alongside its console narration and
// the read-only viewer (cmd/watch) tails. It is a leaf package — stdlib only, no
// harness imports — so every narration surface (runlog, the loop's own narrator)
// and the viewer can depend on it without a cycle.
//
// The stream lives at agent-harness/logs/loop.jsonl, one JSON Record per line. It
// is GLOBAL (across every ticket and stage, unlike the per-ticket run.jsonl) and
// bounded to one daemon run: the daemon truncates it at clean startup, like
// loop.log. Each record's Message is the console line verbatim, so a record is a
// strict superset of the line it mirrors; Kind is a closed enum the viewer
// switches on without re-parsing prose.
package loopstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// FileName is the global stream's fixed filename under the logs root.
const FileName = "loop.jsonl"

// PathUnder is the global stream's path for a given logs root — the single source
// of truth shared by the per-ticket logger and the cmd entrypoints, so the daemon
// (which truncates it) and the viewer (which tails it) never disagree on the path.
func PathUnder(logsRoot string) string { return filepath.Join(logsRoot, FileName) }

// Kind is the closed enum of narrated event types the viewer switches on. Adding
// a value here is a deliberate widening of the daemon→viewer contract: the viewer
// must handle every Kind (its exhaustiveness is asserted by a test), so a new kind
// that the viewer doesn't map is a test failure, never a silent default (ADR-0005).
type Kind string

const (
	// KindTicketSelected — a ticket was selected and claimed off the queue.
	KindTicketSelected Kind = "ticket-selected"
	// KindStageStart — one of the three pipeline stages began (Stage names which).
	KindStageStart Kind = "stage-start"
	// KindSandboxLaunch — a sandboxed agent session container is launching.
	KindSandboxLaunch Kind = "sandbox-launch"
	// KindToolUse — the running agent session invoked a tool (activity heartbeat).
	KindToolUse Kind = "tool-use"
	// KindSessionResult — an agent session ended (success or error).
	KindSessionResult Kind = "session-result"
	// KindPROpened — the review stage pushed the branch and opened a PR.
	KindPROpened Kind = "pr-opened"
	// KindTicketReleased — a claimed ticket was released back to Todo (no PR).
	KindTicketReleased Kind = "ticket-released"
	// KindCapAbort — an Anthropic spending-cap aborted a run before it could ship.
	KindCapAbort Kind = "cap-abort"
	// KindCapBackoff — the daemon is sleeping out the post-cap-abort backoff: the
	// entry (with duration + wake time), each periodic heartbeat, and the wake/re-poll
	// are all this kind, so a long backoff stays observable instead of a black hole
	// (BEH-605). Distinct from KindCapAbort, which marks the abort itself.
	KindCapBackoff Kind = "cap-backoff"
	// KindBreakerTrip — the circuit breaker tripped and the daemon is winding down.
	KindBreakerTrip Kind = "breaker-trip"
	// KindIdle — the queue was empty and the daemon is idling before re-polling.
	KindIdle Kind = "idle"
)

// AllKinds is the closed set, in a stable order. It exists so the viewer's
// kind→render mapping can be exhaustiveness-checked by a test (ADR-0005): if a new
// Kind is added above without a viewer mapping, that test fails.
func AllKinds() []Kind {
	return []Kind{
		KindTicketSelected,
		KindStageStart,
		KindSandboxLaunch,
		KindToolUse,
		KindSessionResult,
		KindPROpened,
		KindTicketReleased,
		KindCapAbort,
		KindCapBackoff,
		KindBreakerTrip,
		KindIdle,
	}
}

// Valid reports whether k is a defined member of the closed enum.
func (k Kind) Valid() bool {
	for _, v := range AllKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// Record is one line of the stream. Message is the console narration verbatim;
// Kind/Ticket/Stage are the structured fields the viewer reads without parsing
// Message. Detail carries optional extra context (unused by the tracer viewer).
type Record struct {
	TS      string `json:"ts"`
	Kind    Kind   `json:"kind"`
	Ticket  string `json:"ticket"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// Stream is an append-only writer over the global loop.jsonl. It holds only a
// path — each Append opens, writes one line, and closes, so concurrent writers
// (the loop's narrator and a per-ticket logger in the same single-threaded daemon)
// never share a file handle and a crash never leaves it open.
type Stream struct {
	path string
}

// NewStream returns a Stream writing to path (typically <logsRoot>/loop.jsonl).
func NewStream(path string) *Stream { return &Stream{path: path} }

// Path is the file the stream writes to.
func (s *Stream) Path() string { return s.path }

// Append marshals r to one JSON line and appends it to the stream, creating the
// file if absent. A record with no TS is stamped with the current UTC time so the
// stream is always ordered; a caller-supplied TS (e.g. matching the console line's
// timestamp) is preserved.
func (s *Stream) Append(r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Truncate empties the stream, bounding it to one daemon run — called at clean
// daemon startup, like loop.log. An absent file is created empty (a no-op
// success), so a first-ever run never fails on the missing file.
func (s *Stream) Truncate() error {
	f, err := os.OpenFile(s.path, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}
