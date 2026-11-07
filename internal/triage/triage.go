// Package triage is the triage loop (ADR-0044): a [loop.Source] over the tracker
// tickets nobody has marked ready for an agent yet, and a [loop.Pipeline] that
// runs one read-only Triage session per ticket and acts on its verdict. A ready
// ticket gets the ready label, and on Linear moves from Backlog to Todo, which puts
// it in the ticket loop's queue. A ticket that is not ready gets one comment
// listing what a human must answer or do first.
//
// Each verdict is recorded against a fingerprint of the ticket's title and
// description, so a judged ticket is not judged again until a human edits it.
package triage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// Backlog is the slice of the tracker the triage loop reads and writes: the
// untriaged tickets, the ready mark, and the comment that carries a verdict.
type Backlog interface {
	tracker.Backlog
	AddComment(key tracker.Key, body string) error
}

// AsBacklog returns the tracker as a Backlog, or an error naming why the tracker
// cannot be triaged: only trackers whose readiness is a label can (ADR-0044).
func AsBacklog(t tracker.Tracker, kind string) (Backlog, error) {
	b, ok := t.(Backlog)
	if !ok {
		return nil, fmt.Errorf("tracker.kind = %q cannot run a triage loop: its ready queue is not a label the harness can add (supported: linear, github)", kind)
	}
	return b, nil
}

// Item is a triage work item's [loop.WorkItem.Context]: the ticket as it was
// listed, and the fingerprint its verdict is recorded against.
type Item struct {
	Ticket      ticket.Ticket
	Fingerprint string
}

// Fingerprint identifies what a verdict was given over: the ticket's title and
// description. A human answering the questions by editing the ticket changes it,
// and the ticket is judged again. Labels and comments are left out: the harness's
// own comment must not invalidate its own verdict.
func Fingerprint(t ticket.Ticket) string {
	sum := sha256.Sum256([]byte(t.Title + "\x00" + t.Description))
	return hex.EncodeToString(sum[:])
}

// Verdict is what a Triage session writes to /findings/verdict.json.
type Verdict struct {
	Ready     bool     `json:"ready"`
	Reason    string   `json:"reason"`
	Questions []string `json:"questions"`
}

// ReadVerdict reads and checks the verdict a session left in dir. It is the
// assess step's ground truth: an absent file means the session never reached a
// verdict, and a verdict that is not ready must say what a human has to do, or the
// comment it becomes would leave the human nothing to act on.
func ReadVerdict(path string) (Verdict, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Verdict{}, fmt.Errorf("the session wrote no verdict")
	}
	if err != nil {
		return Verdict{}, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Verdict{}, fmt.Errorf("the verdict is not a JSON object: %w", err)
	}
	if _, ok := probe["ready"]; !ok {
		return Verdict{}, fmt.Errorf("the verdict has no \"ready\" field")
	}
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return Verdict{}, fmt.Errorf("the verdict does not match {ready, reason, questions}: %w", err)
	}
	v.Reason = strings.TrimSpace(v.Reason)
	var qs []string
	for _, q := range v.Questions {
		if q = strings.TrimSpace(q); q != "" {
			qs = append(qs, q)
		}
	}
	v.Questions = qs
	if !v.Ready && len(v.Questions) == 0 {
		return Verdict{}, fmt.Errorf("the verdict is not ready but lists no question for a human")
	}
	return v, nil
}

// Record is one recorded verdict.
type Record struct {
	Fingerprint string    `json:"fingerprint"`
	Ready       bool      `json:"ready"`
	Reason      string    `json:"reason"`
	Questions   []string  `json:"questions,omitempty"`
	Applied     bool      `json:"applied"`
	At          time.Time `json:"at"`
}

// Store keeps the recorded verdicts, one file per ticket, beside the other state
// a Consumer's loops share (config.LocksDir).
type Store struct {
	Dir string
}

// StoreFor is the Store under a Consumer checkout.
func StoreFor(checkoutPath string) Store {
	return Store{Dir: filepath.Join(config.LocksDir(checkoutPath), "triage")}
}

func (s Store) path(key string) string {
	return filepath.Join(s.Dir, ticket.Slug(key)+".json")
}

// Judged reports whether a verdict was recorded for the ticket at this
// fingerprint. A record that cannot be read counts as none: judging a ticket again
// costs a session, while skipping it forever would hide it.
func (s Store) Judged(key, fingerprint string) bool {
	raw, err := os.ReadFile(s.path(key))
	if err != nil {
		return false
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	return r.Fingerprint == fingerprint
}

// Save records a verdict for the ticket, replacing any earlier one.
func (s Store) Save(key string, r Record) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(key))
}

// commentMarker tags the harness's triage comments, so a reader (or a later
// tool) can tell them from a person's.
const commentMarker = "<!-- agent-harness:triage -->"

// notReadyComment is the comment a ticket that is not ready gets: why, and each
// thing a human must answer or do, with the two ways forward.
func notReadyComment(v Verdict, readyLabel string) string {
	var b strings.Builder
	b.WriteString("**Triage: not ready for an agent yet.**")
	if v.Reason != "" {
		b.WriteString(" " + v.Reason)
	}
	b.WriteString("\n\nBefore an agent can start, a human needs to settle:\n")
	for _, q := range v.Questions {
		b.WriteString("\n- " + q)
	}
	b.WriteString("\n\nEdit the ticket's description to answer these, and triage judges it again on its next poll.")
	if readyLabel != "" {
		b.WriteString(" Or add the `" + readyLabel + "` label yourself to queue it as it stands.")
	}
	b.WriteString("\n\n" + commentMarker)
	return b.String()
}

// readyComment is the note a ticket marked ready gets, so the label has a reason
// beside it.
func readyComment(v Verdict) string {
	s := "**Triage: ready for an agent** — queued for the ticket loop."
	if v.Reason != "" {
		s += " " + v.Reason
	}
	return s + "\n\n" + commentMarker
}
