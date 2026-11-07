package stages

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sentry"
)

// herdTicket is the incident #211 was filed from: a herd ticket whose first step
// is reading a Sentry issue, and which also names the Linear tickets it was seen
// in (BEH-1148, BEH-1179) — tracker Keys, not Sentry issues.
const herdTicket = "Seen twice: BEH-1148 and BEH-1179.\n\n1. Read the Sentry events for HERD-3P and measure `herd.secrets.load`.\n2. Fix it."

// cannedHERD3P is what a Sentry host returns for HERD-3P: the event's formatted
// body still carries the user's email and IP, as Sentry's does.
var cannedHERD3P = sentry.Issue{
	ID: "4504", ShortID: "HERD-3P", Title: "TypeError: secrets is undefined", Culprit: "secrets.load",
	Permalink: "https://acme.sentry.io/issues/4504/", Level: "error", Status: "unresolved",
	Count: "128", UserCount: 17, FirstSeen: "2026-11-01T10:00:00Z", LastSeen: "2026-11-06T09:30:00Z",
	Event: "## Exception\n\nat secrets.load (secrets.ts:42)\n\n## User\n\n- email: jane@example.com\n- ip_address: 203.0.113.7\n\n## Tags\n\n- user: email:jane@example.com\n- client_ip: 203.0.113.7\n- release: herd@1.4.0",
}

func sentryCfg() config.Config {
	cfg := stageCfg()
	cfg.Sentry = &config.SentryConfig{Org: "acme", Projects: []string{"HERD"}, BaseURL: "https://sentry.io"}
	return cfg
}

func citing(h *hostio.Fake, body string) {
	h.Trk.Ticket.Identifier = "PROJ-1"
	h.Trk.Ticket.Description = body
}

func TestImplementationInjectsTheCitedSentryIssueScrubbed(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	citing(h, herdTicket)
	h.SentryIssues = map[string]sentry.Issue{"HERD-3P": cannedHERD3P}
	h.AgentFn = commitsHandoff(h, repo)

	res := Implementation(h, sentryCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	p := h.Agents[0].Prompt
	for _, want := range []string{"Sentry context (already fetched for you — do not look it up):", "snapshot", "HERD-3P: TypeError: secrets is undefined", "secrets.ts:42", "release: herd@1.4.0"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, leak := range []string{"jane@example.com", "203.0.113.7"} {
		if strings.Contains(p, leak) {
			t.Errorf("prompt leaks %q", leak)
		}
	}
	// BEH-1148 and BEH-1179 are tracker Keys: only HERD-3P was looked up.
	if got := sentryCalls(h); len(got) != 1 || got[0] != "sentry HERD-3P" {
		t.Errorf("sentry calls = %v, want HERD-3P only", got)
	}
}

func TestReviewInjectsTheCitedSentryIssue(t *testing.T) {
	h, _ := handedOff(t, "PROJ-1")
	citing(h, herdTicket)
	h.SentryIssues = map[string]sentry.Issue{"HERD-3P": cannedHERD3P}

	Review(h, sentryCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if len(h.Agents) == 0 || !strings.Contains(h.Agents[0].Prompt, "HERD-3P: TypeError: secrets is undefined") {
		t.Fatalf("review prompt carries no Sentry section: %v", labels(h.Agents))
	}
}

// Without a [sentry] table nothing is looked up and the prompt is byte-identical
// to one built for a ticket that cites nothing.
func TestWithoutSentryConfigThePromptIsUnchanged(t *testing.T) {
	prompts := map[string]string{}
	for name, body := range map[string]string{"cites": herdTicket, "plain": "Seen twice: BEH-1148 and BEH-1179.\n\n1. Read the Sentry events for (redacted) and measure `herd.secrets.load`.\n2. Fix it."} {
		h, repo := fresh(t, "PROJ-1")
		citing(h, body)
		h.SentryIssues = map[string]sentry.Issue{"HERD-3P": cannedHERD3P}
		h.AgentFn = commitsHandoff(h, repo)
		Implementation(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})
		if got := sentryCalls(h); len(got) != 0 {
			t.Errorf("%s: sentry calls = %v, want none without [sentry]", name, got)
		}
		prompts[name] = strings.Replace(h.Agents[0].Prompt, body, "<body>", 1)
	}
	if prompts["cites"] != prompts["plain"] {
		t.Error("a ticket citing HERD-3P changed the prompt with Sentry off")
	}
	if strings.Contains(prompts["cites"], "Sentry context") {
		t.Error("the prompt has a Sentry section with Sentry off")
	}
}

// A fetch that fails never blocks dispatch: the run logs the advisory, the
// envelope names the issue as not fetched, and the session still launches.
func TestASentryFetchFailureIsAnAdvisoryNotABlock(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	citing(h, herdTicket)
	h.SentryErr = errors.New("SENTRY_AUTH_TOKEN is not set on the host")
	h.AgentFn = commitsHandoff(h, repo)
	log := stageLog(t, "PROJ-1")

	res := Implementation(h, sentryCfg(), log, Args{Identifier: "PROJ-1"})

	if !res.OK || len(h.Agents) != 1 {
		t.Fatalf("a Sentry failure must not block dispatch: res=%+v agents=%v", res, labels(h.Agents))
	}
	if want := "⚠ PROJ-1 cites Sentry HERD-3P but it could not be fetched: SENTRY_AUTH_TOKEN is not set on the host"; !strings.Contains(narration(t, log), want) {
		t.Errorf("narration missing %q", want)
	}
	if p := h.Agents[0].Prompt; !strings.Contains(p, "could not fetch these Sentry issues the ticket cites: HERD-3P (SENTRY_AUTH_TOKEN is not set on the host)") {
		t.Errorf("prompt does not name the unfetched issue:\n%s", p)
	}
}

func sentryCalls(h *hostio.Fake) []string {
	var out []string
	for _, c := range h.Calls {
		if strings.HasPrefix(c, "sentry ") {
			out = append(out, c)
		}
	}
	return out
}
