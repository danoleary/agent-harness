package prompt

import (
	"fmt"
	"strings"

	"github.com/danoleary/agent-harness/internal/sentry"
)

// sentrySection renders the Sentry issues the ticket cites, as the host fetched
// them at dispatch (#211, ADR-0041). The sandbox reaches only Anthropic
// (ADR-0002), so a session told to "read the Sentry events for HERD-3P" could not,
// and shipped a fix without the measurement. The issues are already scrubbed of
// user data (internal/sentry); the text is still external, so it is defanged like
// the ticket body. A cited issue the host could not fetch is named, so the session
// reports the measurement as missing instead of guessing it. Empty when the
// feature is off or the ticket cites nothing.
func sentrySection(s *sentry.Snapshot) string {
	if s == nil || (len(s.Issues) == 0 && len(s.Missing) == 0) {
		return ""
	}
	var b strings.Builder
	b.WriteString("Sentry context (already fetched for you — do not look it up):")
	b.WriteString("\n\nThe harness read the Sentry issues this ticket cites on the host when it dispatched this session. This is a snapshot from that time, not live data: counts and last-seen times may have moved since. The sandbox cannot reach Sentry, so do not try. User data (the event's user, emails and IP addresses) was removed before it reached you.")
	for _, i := range s.Issues {
		name := i.ShortID
		if name == "" {
			name = "issue " + i.ID
		}
		fmt.Fprintf(&b, "\n\n## %s: %s", name, i.Title)
		fmt.Fprintf(&b, "\n\n- status: %s, level: %s", orUnknown(i.Status), orUnknown(i.Level))
		fmt.Fprintf(&b, "\n- events: %s, users affected: %d", orUnknown(i.Count), i.UserCount)
		fmt.Fprintf(&b, "\n- first seen: %s, last seen: %s", orUnknown(i.FirstSeen), orUnknown(i.LastSeen))
		if i.Culprit != "" {
			b.WriteString("\n- culprit: " + i.Culprit)
		}
		if i.Permalink != "" {
			b.WriteString("\n- link: " + i.Permalink)
		}
		if strings.TrimSpace(i.Event) != "" {
			b.WriteString("\n\nIts recommended event:\n\n~~~~markdown\n" + strings.TrimRight(i.Event, "\n") + "\n~~~~")
		}
	}
	if len(s.Missing) > 0 {
		named := make([]string, len(s.Missing))
		for i, m := range s.Missing {
			named[i] = m.Ref + " (" + m.Reason + ")"
		}
		b.WriteString("\n\nThe harness could not fetch these Sentry issues the ticket cites: " + strings.Join(named, ", ") + ". Do not guess their data. If the work needs that measurement, say in your handoff that it is missing and why.")
	}
	return defang(b.String())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
