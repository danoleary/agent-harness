// Package hostlock is the exclusion several loop instances on one host share
// (#89): a lock file every instance of a Consumer opens, so two daemons that
// select at the same moment claim one at a time, and a bounded set of gate slots,
// so two runs never execute their gates at once past the limit.
//
// The locks are flock(2) locks on files under the Consumer's .agent-harness/
// directory. A flock belongs to the open file, not the process, so two daemons
// and two goroutines in one test see the same exclusion, and the kernel drops a
// dead daemon's lock with its file descriptors: there is no stale lock to reap.
package hostlock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// File is an exclusive lock on one file, taken by every instance that must not
// overlap with another.
type File string

// Lock blocks until this caller holds the lock, creating the file and its
// directory if absent. The returned func releases it.
func (f File) Lock() (unlock func(), err error) {
	file, err := open(string(f))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock %s: %w", f, err)
	}
	return func() { release(file) }, nil
}

// Slots bounds how many holders run at once: Limit slot files under Dir, each
// held by at most one. A non-positive Limit is no limit.
type Slots struct {
	Dir   string
	Limit int
	// Poll is how long a waiting caller sleeps between tries; zero is one second.
	Poll time.Duration
}

// Acquire blocks until the caller holds a slot and returns the func that frees
// it. onWait, when set, is called once if every slot is taken on the first try,
// so a caller can say why it is not moving.
func (s Slots) Acquire(onWait func()) (release func(), err error) {
	if s.Limit <= 0 {
		return func() {}, nil
	}
	poll := s.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for waited := false; ; waited = true {
		for i := 0; i < s.Limit; i++ {
			held, err := s.try(i)
			if err != nil {
				return nil, err
			}
			if held != nil {
				return held, nil
			}
		}
		if !waited && onWait != nil {
			onWait()
		}
		time.Sleep(poll)
	}
}

// try takes slot i without blocking; nil, nil means another holder has it.
func (s Slots) try(i int) (func(), error) {
	file, err := open(filepath.Join(s.Dir, fmt.Sprintf("gate-%d.lock", i)))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, nil
		}
		return nil, fmt.Errorf("lock %s: %w", file.Name(), err)
	}
	return func() { release(file) }, nil
}

// open opens (creating) a lock file, and the directory it lives in.
func open(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

// release drops the lock and closes the file. Closing alone would drop it; the
// explicit unlock keeps the order obvious.
func release(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	file.Close()
}
