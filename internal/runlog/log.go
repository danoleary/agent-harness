// Package runlog writes a ticket-keyed log directory: concise console narration
// mirrored to run.jsonl, plus raw agent transcripts. The directory is keyed by
// ticket id (not run id) so every session a ticket sees — implementation,
// review, retrospective — lands under one dir a later, separately-invoked tool
// can find by globbing (DESIGN.md "Logging").
package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Logger writes one ticket's logs under logs/<ticket-id>/.
type Logger struct {
	// Dir is the absolute path of this ticket's log directory.
	Dir string
}

// MakeRunID builds a sortable, filesystem-safe run id from a timestamp,
// e.g. "20260611-140815". It is the per-session suffix on transcript filenames,
// not the directory key.
func MakeRunID(now time.Time) string {
	return now.Format("20060102-150405")
}

// New creates the ticket's log directory and returns a Logger writing into it.
// ticketID is the human identifier (e.g. "BEH-370").
func New(logsRoot, ticketID string) (*Logger, error) {
	dir := filepath.Join(logsRoot, ticketID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{Dir: dir}, nil
}

// TranscriptName is the filename for a session's stream-json transcript:
// session-prefixed and run-id-suffixed (e.g. "implementation-20260611-140805.jsonl").
// The session prefix names which tool ran; the run-id suffix keeps a second run
// of the same ticket from clobbering the first.
func TranscriptName(session, runID string) string {
	return session + "-" + runID + ".jsonl"
}

// StepLogName is the filename for a raw-stdout step log — the install/gate
// commands the review tool runs around the agent session. Their content is piped
// tool stdout (pnpm install / pnpm check), NOT a stream-json event stream, so
// they carry a ".log" suffix rather than ".jsonl": a reader who sees ".jsonl"
// expects parseable JSON and wastes turns discovering it is plain text (BEH-537).
// Naming is step-prefixed and run-id-suffixed, non-clobbering like TranscriptName.
func StepLogName(step, runID string) string {
	return step + "-" + runID + ".log"
}

// GateTranscriptName is the filename for the review tool's host-side gate re-run
// log — a raw-stdout step log (see StepLogName).
func GateTranscriptName(runID string) string {
	return StepLogName("gate", runID)
}

// StepFooter is the self-describing terminal line appended to a raw-stdout step
// log (install/gate) once the step exits. Those logs are piped tool stdout that
// truncates at the kill point with no marker, so without this an OOM-kill is
// indistinguishable from a clean finish unless you cross-reference run.jsonl
// (BEH-537). 137 is a SIGKILL (128+9) — under the harness that is almost always
// an OOM-kill or a wall-clock/spend-cap reap, so call it out by name.
func StepFooter(exitCode int) string {
	switch exitCode {
	case 0:
		return "-- step exited 0 (ok) --"
	case 137:
		return "-- step exited 137 (SIGKILL — likely OOM-kill or wall-clock/spend cap) --"
	default:
		return fmt.Sprintf("-- step exited %d --", exitCode)
	}
}

// FindingsDir is the path of a session's findings dropbox dir, under the ticket
// dir (logs/<ticket-id>/findings/<session>/). It is not created — the caller
// mounts it and is responsible for MkdirAll.
func (l *Logger) FindingsDir(session string) string {
	return filepath.Join(l.Dir, "findings", session)
}

// Event emits one concise narration line to the console and mirrors it to run.jsonl.
func (l *Logger) Event(message string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("%s  %s\n", ts, message)
	record, _ := json.Marshal(struct {
		TS      string `json:"ts"`
		Message string `json:"message"`
	}{TS: ts, Message: message})
	l.append("run.jsonl", string(record)+"\n")
}

// TeeLine appends a raw transcript line (e.g. a claude stream-json chunk) to a
// file under the ticket dir (the caller picks the per-session filename via
// TranscriptName so concurrent sessions don't share a file).
func (l *Logger) TeeLine(file, raw string) {
	if !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	l.append(file, raw)
}

func (l *Logger) append(file, content string) {
	f, err := os.OpenFile(
		filepath.Join(l.Dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644,
	)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}
