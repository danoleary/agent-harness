package viewer

import (
	"bytes"
	"io"
	"os"
)

// follower is the stdlib-only file-follow engine shared by the plain Tailer and
// the DashboardTailer: across repeated next() calls it yields the bytes appended
// to a stream file since the last call. It owns only an offset, so the rendering
// concern (plain lines vs. dashboard model) stays in its caller.
type follower struct {
	path   string
	offset int64
}

// next returns the complete (newline-terminated) bytes appended since the previous
// call, advancing the offset past them. running=false means the file is absent (no
// daemon/run has written it). reset=true means the file shrank since the last call
// — a daemon restart truncating the bounded stream — so the offset has been rewound
// and the returned bytes are from the fresh file's start; the caller must reset any
// derived state. A trailing partial (not yet newline-terminated) line is held back
// until it completes, so half a JSON record is never returned.
func (f *follower) next() (data []byte, running, reset bool, err error) {
	info, err := os.Stat(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, false, nil
		}
		return nil, false, false, err
	}

	if info.Size() < f.offset {
		f.offset = 0
		reset = true
	}

	file, err := os.Open(f.path)
	if err != nil {
		return nil, true, reset, err
	}
	defer file.Close()

	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return nil, true, reset, err
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, true, reset, err
	}

	nl := bytes.LastIndexByte(raw, '\n')
	if nl < 0 {
		return nil, true, reset, nil
	}
	complete := raw[:nl+1]
	f.offset += int64(len(complete))
	return complete, true, reset, nil
}

// Tailer follows the loop.jsonl stream file, rendering newly-appended records to a
// writer across repeated Poll calls. It owns a single View, so ticket/stage state
// is carried forward across polls — exactly what the daemon→viewer contract needs
// (ADR-0005). It is the engine cmd/watch loops on a ticker; the time/loop concern
// stays in the thin command, keeping this unit-testable.
type Tailer struct {
	follow follower
	view   *View
}

// NewTailer returns a Tailer over the stream file at path.
func NewTailer(path string) *Tailer {
	return &Tailer{follow: follower{path: path}, view: New()}
}

// Poll renders any complete lines appended since the last Poll. running=false means
// the file is absent (no daemon/run has written it) — the caller surfaces the
// not-running status. A file that shrank since the last poll is treated as a fresh
// daemon run: the offset and view reset so the new run renders from its start.
func (t *Tailer) Poll(out io.Writer) (running bool, err error) {
	data, running, reset, err := t.follow.next()
	if reset {
		t.view = New()
	}
	if err != nil || !running || len(data) == 0 {
		return running, err
	}
	return true, t.view.Replay(bytes.NewReader(data), out)
}
