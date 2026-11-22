package stages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// BlockedMarker is the file an implementation session writes into its findings
// dropbox (mounted at /findings), beside ResolvedMarker, for the two outcomes a
// handoff commit cannot carry (#368, ADR-0070). Its shape is
// {"reason": "...", "questions": ["..."], "follow_ups": [{"title": "...", "body": "..."}]}.
// A reason means the ticket is blocked on a human: the run is SessionBlocked. Each
// follow-up is a further ticket the work needs, such as a split's second half;
// it may come with a normal handoff.
const BlockedMarker = "blocked.json"

// blockedReport is the session's blocked marker as it wrote it.
type blockedReport struct {
	Reason    string     `json:"reason"`
	Questions []string   `json:"questions"`
	FollowUps []followUp `json:"follow_ups"`
}

// followUp is one further ticket the session asks the harness to file.
type followUp struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// clearMarkers removes the resolved and blocked markers an earlier run of the
// ticket left in its findings dir, which is keyed on the ticket, not the run, so
// a stale marker cannot settle this run. An absent marker is a no-op.
func clearMarkers(findingsDir string) error {
	for _, m := range []string{ResolvedMarker, BlockedMarker} {
		if err := os.Remove(filepath.Join(findingsDir, m)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// readBlocked reads the session's blocked marker. ok is false when there is
// none. A marker that is not the documented JSON is an error, and reads as no
// marker to the caller.
func readBlocked(findingsDir string) (rep blockedReport, ok bool, err error) {
	raw, rErr := os.ReadFile(filepath.Join(findingsDir, BlockedMarker))
	if errors.Is(rErr, os.ErrNotExist) {
		return blockedReport{}, false, nil
	}
	if rErr != nil {
		return blockedReport{}, true, rErr
	}
	if jErr := json.Unmarshal(raw, &rep); jErr != nil {
		return blockedReport{}, true, fmt.Errorf("it is not {reason, questions, follow_ups} JSON: %w", jErr)
	}
	rep.Reason = strings.TrimSpace(rep.Reason)
	var qs []string
	for _, q := range rep.Questions {
		if q = strings.TrimSpace(q); q != "" {
			qs = append(qs, q)
		}
	}
	rep.Questions = qs
	return rep, true, nil
}

// blockedResult is the SessionBlocked Result a marker with a reason gives, and
// whether it gives one.
func (rep blockedReport) blockedResult() (Result, bool) {
	if rep.Reason == "" {
		return Result{}, false
	}
	return Result{Disposition: SessionBlocked, BlockedWhy: rep.Reason, BlockedQuestions: rep.Questions}, true
}

// followUpFindings turns the marker's follow-ups into findings for the Consumer's
// tracker. Each is NeedsHuman, so it files without the ready label, behind a
// human, as a finding with needs_human does (ADR-0033). An entry with no title is
// dropped: the tracker would have nothing to call it.
func (rep blockedReport) followUpFindings(worked string) []findings.Finding {
	var fs []findings.Finding
	for _, fu := range rep.FollowUps {
		title := strings.TrimSpace(fu.Title)
		if title == "" {
			continue
		}
		body := strings.TrimSpace(fu.Body)
		if body != "" {
			body += "\n\n"
		}
		body += "_Follow-up the implementation session of " + worked + " asked for (#368)._"
		fs = append(fs, findings.Finding{
			Title: title, Body: body, Audience: findings.AudienceProject,
			Kind: "follow-up", NeedsHuman: true,
		})
	}
	return fs
}

// fileFollowUps files the marker's follow-ups through the findings filing path,
// each linked to the worked ticket and not ready. It is best-effort, as all
// filing is: a failure is narrated and the run's verdict stands.
func fileFollowUps(h hostio.Host, log *runlog.Logger, t ticket.Ticket, findingsDir string, rep blockedReport) {
	fs := rep.followUpFindings(t.Identifier)
	if len(fs) == 0 {
		return
	}
	if err := filing.WriteDropbox(findingsDir, fs); err != nil {
		log.Event("⚠ could not write the session's follow-up tickets for filing (" + err.Error() + ") — file them by hand from " + BlockedMarker + " in " + findingsDir)
		return
	}
	log.Event(fmt.Sprintf("tdd · filing %d follow-up ticket(s) the session asked for, linked to %s and not ready (#368)", len(fs), t.Identifier))
	h.FileFindings(findingsDir, t.TeamID, t.Identifier)
}
