// Package nowork is the record of the last implementation run that found nothing
// to hand off: per ticket, the origin/main SHA the run worked against and the
// ticket as selection read it (#330, ADR-0053). The implementation stage writes it
// when a session ran to completion and left no commit ahead of origin/main, and
// clears it on any other end. Selection reads it and passes over the ticket while
// main and the ticket are both unchanged, because a second run would find the same
// nothing for the same reason. A run that found the ticket already resolved on
// main also names the resolving commit, and selection passes over the ticket for
// as long as main still has that commit, however far main moves (#453). That
// verdict outlives the loop's blocked label, so a later run of the ticket is told
// it (#480).
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
	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Fingerprint identifies what a ticket said when selection read it: its title,
// body and labels. A human who edits any of them has given the next run something
// the last one did not have.
type Fingerprint string

// Of is t's fingerprint. Label order does not count, and neither does anything
// else selection reads (priority, URL), which says nothing about the work. Nor
// does the occurrence count a retrospective bumps in a filed finding's body: the
// harness wrote it, not a human (#480).
func Of(t ticket.Ticket) Fingerprint {
	labels := slices.Clone(t.Labels)
	slices.Sort(labels)
	raw, _ := json.Marshal(struct {
		Title, Body string
		Labels      []string
	}{t.Title, findings.WithoutOccurrences(t.Description), labels})
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
	// ResolvedBy is the full SHA of the origin/main commit the run found already
	// resolving the ticket (ResolvedOnMain or AlreadyOnMain), or empty. A main
	// that moved still holds that fix while the commit is on it (#453).
	ResolvedBy string `json:"resolved_by,omitempty"`
	// Why is the session's reason that ResolvedBy fixes the ticket, or the
	// subject of the commit the own-key scan matched; a later run is told it (#480).
	Why string `json:"why,omitempty"`
	// Held is set once the loop's blocked label landed on the ticket: the label
	// holds it from then on, and a selection that offers it again means a human
	// removed the label (#480).
	Held bool `json:"held,omitempty"`
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
	return r.WriteResolved(key, base, fp, "", "")
}

// WriteResolved is Write for a run that found key already resolved on main by
// the commit by, for the reason why (#453). An empty by is a plain Write.
func (r Records) WriteResolved(key, base string, fp Fingerprint, by, why string) error {
	return r.put(key, Record{Base: base, Ticket: fp, At: r.now(), ResolvedBy: by, Why: why})
}

// Hold notes that the loop's blocked label landed on key. The label holds the
// ticket from then on, so a plain record is cleared; a resolved one is kept,
// marked held, so the run a human's removal of the label dispatches is told the
// verdict (#480). Best-effort, like Clear.
func (r Records) Hold(key string) {
	rec, ok := r.Read(key)
	if !ok || rec.ResolvedBy == "" {
		r.Clear(key)
		return
	}
	rec.Held = true
	if err := r.put(key, rec); err != nil {
		r.Clear(key)
	}
}

// put writes rec as key's record. The rename makes it atomic.
func (r Records) put(key string, rec Record) error {
	raw, err := json.Marshal(rec)
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
