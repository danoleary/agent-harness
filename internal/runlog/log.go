// Package runlog writes a per-run log directory: concise console narration
// mirrored to run.jsonl, plus raw agent transcripts.
package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Logger writes one run's logs under logs/<run-id>/.
type Logger struct {
	// RunDir is the absolute path of this run's log directory.
	RunDir string
}

// MakeRunID builds a sortable, filesystem-safe run id from a timestamp,
// e.g. "20260611-140815".
func MakeRunID(now time.Time) string {
	return now.Format("20060102-150405")
}

// New creates the run's log directory and returns a Logger writing into it.
func New(logsRoot, runID string) (*Logger, error) {
	runDir := filepath.Join(logsRoot, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{RunDir: runDir}, nil
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
// per-run file.
func (l *Logger) TeeLine(file, raw string) {
	if !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	l.append(file, raw)
}

func (l *Logger) append(file, content string) {
	f, err := os.OpenFile(
		filepath.Join(l.RunDir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644,
	)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}
