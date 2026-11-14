// Package nowork is the record of the last implementation run that found nothing
// to hand off: per ticket, the origin/main SHA the run worked against and the
// ticket as selection read it (#330, ADR-0053). The implementation stage writes it
// when a session ran to completion and left no commit ahead of origin/main, and
// clears it on any other end. Selection reads it and passes over the ticket while
// main and the ticket are both unchanged, because a second run would find the same
// nothing for the same reason.
package nowork

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Fingerprint identifies what a ticket said when selection read it: its title,
// body and labels. A human who edits any of them has given the next run something
// the last one did not have.
type Fingerprint string

// Of is t's fingerprint. Label order does not count, and neither does anything
// else selection reads (priority, URL), which says nothing about the work.
func Of(t ticket.Ticket) Fingerprint {
	labels := slices.Clone(t.Labels)
	slices.Sort(labels)
	raw, _ := json.Marshal(struct {
		Title, Body string
		Labels      []string
	}{t.Title, t.Description, labels})
	sum := sha256.Sum256(raw)
	return Fingerprint(hex.EncodeToString(sum[:]))
}

// Record is one run that found nothing to hand off.
type Record struct {
	// Base is the origin/main SHA the run worked against.
	Base string `json:"base"`
	// Ticket is the ticket's fingerprint as the selection that dispatched the run
	// read it.
	Ticket Fingerprint `json:"ticket"`
	// At is when the run recorded it, for the operator reading the file.
	At time.Time `json:"at"`
}

// Records is the per-ticket record every loop instance of one Consumer shares
// (config.NoWorkDir), so an instance that did not run the ticket passes it over
// too.
type Records struct {
	dir string
	now func() time.Time
}

// For is the record of cfg's Consumer.
func For(cfg config.Config) Records {
	return Records{dir: config.NoWorkDir(cfg.ProjectPath), now: time.Now}
}

// In is a record kept in dir, on the clock now; for tests.
func In(dir string, now func() time.Time) Records { return Records{dir: dir, now: now} }

func (r Records) path(key string) string { return filepath.Join(r.dir, ticket.Slug(key)) }

// Read returns key's record. ok is false when there is none: never recorded,
// cleared, or unreadable. An unreadable record dispatches the ticket, as before
// the record existed.
func (r Records) Read(key string) (Record, bool) {
	raw, err := os.ReadFile(r.path(key))
	if err != nil {
		return Record{}, false
	}
	var got Record
	if err := json.Unmarshal(raw, &got); err != nil || got.Base == "" {
		return Record{}, false
	}
	return got, true
}

// Write records that key's run found nothing to hand off against base, for the
// ticket selection read as fp. The rename makes it atomic, so a concurrent reader
// sees the old record or the new one, never half of one.
func (r Records) Write(key, base string, fp Fingerprint) error {
	raw, err := json.Marshal(Record{Base: base, Ticket: fp, At: r.now()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dir, ticket.Slug(key)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.path(key))
}

// Clear forgets key's record: its last run handed something off, or ended for a
// reason that says nothing about the work. Forgetting a record that is not there
// is a no-op, and a failed removal leaves a record selection reads only while
// main and the ticket stand still.
func (r Records) Clear(key string) { os.Remove(r.path(key)) }
