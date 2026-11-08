package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/scaffold"
)

// checkSecurityAlerts checks, when the project config declares a
// github_security_alerts loop (ADR-0040), that GH_TOKEN can read the Dependabot
// alerts of the checkout's origin — the read every poll of that loop makes. One
// alert is enough to tell (per_page=1). A 403 is the token's scope (classic:
// security_events) or permission (fine-grained: Dependabot alerts: Read), or
// alerts turned off on the repository; GitHub's message says which. The built-in
// security-alerts loop runs only when named, so it alone adds no line. ok is false
// when no such loop is declared and there is nothing to check.
func checkSecurityAlerts(p Probes, pr project) (Result, bool) {
	const name = "Dependabot alerts"
	if !pr.loaded || !declaresSecurityLoop(pr.cfg) {
		return Result{}, false
	}
	token := p.Getenv("GH_TOKEN")
	if token == "" {
		return Result{Name: name, Status: Skip, Detail: "needs GH_TOKEN"}, true
	}
	out, err := p.Run("git", "-C", pr.path, "remote", "get-url", "origin")
	remote := strings.TrimSpace(string(out))
	repo := scaffold.GitHubRepo(remote)
	if err != nil || repo == "" {
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("origin (%s) is not a GitHub remote; the security-alerts loop reads the Dependabot alerts of origin's repository", orNone(remote)),
			Fix:    "point origin at the project's GitHub repository: git remote set-url origin https://github.com/<owner>/<repo>.git"}, true
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: repo + "; not verified (--offline)"}, true
	}

	req, err := http.NewRequest(http.MethodGet, p.GitHubAPI+"/repos/"+repo+"/dependabot/alerts?state=open&per_page=1", nil)
	if err != nil {
		return unreachable(name, "GitHub", err), true
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("accept", "application/vnd.github+json")
	res, err := p.HTTP.Do(req)
	if err != nil {
		return unreachable(name, "GitHub", err), true
	}
	defer res.Body.Close()
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)

	scopes, classic := res.Header["X-Oauth-Scopes"]
	switch {
	case res.StatusCode < 300:
		return Result{Name: name, Status: OK, Detail: "GH_TOKEN can read the Dependabot alerts on " + repo}, true
	case strings.Contains(strings.ToLower(body.Message), "disabled"):
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("Dependabot alerts are disabled on %s (%s)", repo, body.Message),
			Fix: fmt.Sprintf("turn them on in the repository's Settings → Code security → Dependabot alerts (https://github.com/%s/settings/security_analysis), or run `gh api -X PUT repos/%s/vulnerability-alerts`", repo, repo)}, true
	case res.StatusCode == http.StatusForbidden && classic:
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("classic token cannot read the Dependabot alerts on %s (has: %s)", repo, orNone(strings.Join(scopes, ","))),
			Fix:    "give GH_TOKEN the security_events scope (the repo scope includes it) at " + config.GitHubTokenURL}, true
	case res.StatusCode == http.StatusForbidden:
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("a fine-grained token cannot read the Dependabot alerts on %s (%s)", repo, orNone(body.Message)),
			Fix:    "grant the token the \"Dependabot alerts: Read-only\" repository permission, or use a classic token with the repo and workflow scopes (" + config.GitHubTokenURL + ")"}, true
	case res.StatusCode == http.StatusNotFound:
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("repo %s not found, or GH_TOKEN cannot see it (HTTP 404)", repo),
			Fix: "check origin names the project's GitHub repository, and that GH_TOKEN's owner has access to it"}, true
	}
	return Result{Name: name, Status: Warn, Detail: fmt.Sprintf("could not verify: GitHub answered HTTP %d", res.StatusCode)}, true
}

// declaresSecurityLoop reports whether config.toml declares a loop on the
// github_security_alerts Source.
func declaresSecurityLoop(cfg config.ProjectConfig) bool {
	for _, l := range cfg.ResolvedLoops {
		if l.Source.Kind == config.SourceGitHubSecurityAlerts {
			return true
		}
	}
	return false
}
