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

// GateTranscriptName is the filename for the review tool's host-side gate re-run
// log. Unlike a session transcript this is plain command output, not stream-json,
// so it carries a ".log" suffix rather than ".jsonl" — but it shares the same
// run-id-suffixed, non-clobbering naming as TranscriptName.
func GateTranscriptName(runID string) string {
	return "gate-" + runID + ".log"
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
