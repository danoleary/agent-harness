package stages

import (
	"fmt"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/sentry"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// sentry fetches, on the host, the Sentry issues t's body cites, for the prompt
// (#211, ADR-0041). Nil without a `[sentry]` table or when the body cites none,
// which leaves the prompt as it was. A cited issue that cannot be fetched — no
// token, an HTTP error, an ID Sentry does not know — is an advisory, never a
// block: the run logs it, the envelope names it, and dispatch goes ahead.
func (f frame) sentry(cfg config.Config, t ticket.Ticket) *sentry.Snapshot {
	sc := cfg.Sentry
	if sc == nil {
		return nil
	}
	refs := sentry.Detect(t.Description, sc.Org, sc.BaseURL, sc.Projects)
	if len(refs) == 0 {
		return nil
	}
	snap := sentry.Gather(refs, f.h.SentryIssue)
	if len(snap.Issues) > 0 {
		names := make([]string, len(snap.Issues))
		for i, issue := range snap.Issues {
			names[i] = issue.ShortID
			if names[i] == "" {
				names[i] = "issue " + issue.ID
			}
		}
		f.log.Event(fmt.Sprintf("fetched Sentry %s for %s — injected into the prompt", strings.Join(names, ", "), t.Identifier))
	}
	for _, m := range snap.Missing {
		f.log.Event(fmt.Sprintf("⚠ %s cites Sentry %s but it could not be fetched: %s", t.Identifier, m.Ref, m.Reason))
	}
	return &snap
}
