package doctor

import (
	"errors"
	"strings"

	"github.com/danoleary/agent-harness/internal/sentry"
)

// sentryTokenURL is where an operator makes the token the `[sentry]` table needs.
const sentryTokenURL = "https://sentry.io/settings/account/api/auth-tokens/"

// checkSentry checks, when the project config has a `[sentry]` table, that
// SENTRY_AUTH_TOKEN is set and that Sentry accepts it for the org (ADR-0041). A
// bad token never blocks a run — each cited issue is reported as not fetched —
// but every such ticket then ships without its measurement, so it fails here.
// ok is false when the table is absent and there is nothing to check.
func checkSentry(p Probes, pr project) (Result, bool) {
	const name = "Sentry"
	if !pr.loaded || pr.cfg.Sentry == nil {
		return Result{}, false
	}
	sc := pr.cfg.Sentry
	fix := "create a token with the event:read, project:read and org:read scopes at " + sentryTokenURL + " and set SENTRY_AUTH_TOKEN to it"
	token := p.Getenv("SENTRY_AUTH_TOKEN")
	if token == "" {
		return Result{Name: name, Status: Fail, Detail: "SENTRY_AUTH_TOKEN is not set; [sentry] reads the issues a ticket cites with it", Fix: fix}, true
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: "org " + sc.Org + "; not verified (--offline)"}, true
	}
	err := sentry.Client{BaseURL: sc.BaseURL, Org: sc.Org, Token: token, HTTP: p.HTTP}.Ping()
	switch {
	case err == nil:
		return Result{Name: name, Status: OK, Detail: "org " + sc.Org + " at " + strings.TrimRight(sc.BaseURL, "/") + ", projects " + strings.Join(sc.Projects, ", ")}, true
	case errors.Is(err, sentry.ErrHTTP):
		return Result{Name: name, Status: Fail, Detail: "Sentry rejected SENTRY_AUTH_TOKEN for org " + sc.Org + ": " + err.Error(), Fix: fix + ", and check sentry.org names the org"}, true
	}
	return unreachable(name, "Sentry", err), true
}
