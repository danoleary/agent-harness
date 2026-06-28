package viewer

import (
	"bytes"
	"io"
	"os"
)

// Tailer follows the loop.jsonl stream file, rendering newly-appended records to a
// writer across repeated Poll calls. It owns a single View, so ticket/stage state
// is carried forward across polls — exactly what the daemon→viewer contract needs
// (ADR-0005). It is the engine cmd/watch loops on a ticker; the time/loop concern
// stays in the thin command, keeping this unit-testable.
type Tailer struct {
	path   string
	view   *View
	offset int64
}

// NewTailer returns a Tailer over the stream file at path.
func NewTailer(path string) *Tailer {
	return &Tailer{path: path, view: New()}
}

// Poll renders any complete lines appended since the last Poll. running=false means
// the file is absent (no daemon/run has written it) — the caller surfaces the
// not-running status. A file that shrank since the last poll is treated as a fresh
// daemon run: the offset and view reset so the new run renders from its start. A
// trailing partial (not yet newline-terminated) line is held back until it
// completes, so half a JSON record never renders.
func (t *Tailer) Poll(out io.Writer) (running bool, err error) {
	info, err := os.Stat(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	// Truncation (daemon restart): the stream is bounded to one run, so a smaller
	// file is a new run — rewind and start its view clean.
	if info.Size() < t.offset {
		t.offset = 0
		t.view = New()
	}

	f, err := os.Open(t.path)
	if err != nil {
		return true, err
	}
	defer f.Close()

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return true, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return true, err
	}

	// Only consume up to the last newline; hold back any trailing partial line.
	nl := bytes.LastIndexByte(data, '\n')
	if nl < 0 {
		return true, nil
	}
	complete := data[:nl+1]
	t.offset += int64(len(complete))
	return true, t.view.Replay(bytes.NewReader(complete), out)
}
