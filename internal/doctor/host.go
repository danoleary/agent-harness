package doctor

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
)

// minGit is the oldest git the harness supports: the pre-push replay uses
// `git cherry-pick --empty=drop`, which landed in git 2.45.
var minGit = [2]int{2, 45}

var gitVersionRE = regexp.MustCompile(`git version (\d+)\.(\d+)`)

func checkDocker(p Probes) Result {
	out, err := p.Run("docker", "info")
	if err != nil {
		return Result{
			Name:   "docker",
			Status: Fail,
			Detail: "daemon not reachable (" + firstLine(out, err) + ")",
			Fix:    "start Docker Desktop or the docker service; if `docker` itself is missing, install it from https://docs.docker.com/get-docker/",
		}
	}
	return Result{Name: "docker", Status: OK, Detail: "daemon reachable"}
}

func checkGit(p Probes) Result {
	const fix = "install git 2.45 or newer (`brew install git` on macOS); the pre-push replay uses `git cherry-pick --empty=drop`"
	out, err := p.Run("git", "--version")
	if err != nil {
		return Result{Name: "git", Status: Fail, Detail: "not runnable (" + firstLine(out, err) + ")", Fix: fix}
	}
	m := gitVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		return Result{Name: "git", Status: Warn, Detail: fmt.Sprintf("cannot read a version from %q", strings.TrimSpace(string(out))), Fix: fix}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	version := m[1] + "." + m[2]
	if major < minGit[0] || (major == minGit[0] && minor < minGit[1]) {
		return Result{Name: "git", Status: Fail, Detail: fmt.Sprintf("%s is older than %d.%d", version, minGit[0], minGit[1]), Fix: fix}
	}
	return Result{Name: "git", Status: OK, Detail: version}
}

func checkGH(p Probes) Result {
	out, err := p.Run("gh", "--version")
	if err != nil {
		return Result{
			Name:   "gh",
			Status: Fail,
			Detail: "not installed (" + firstLine(out, err) + ")",
			Fix:    "install the GitHub CLI from https://cli.github.com; the harness opens and watches pull requests with it",
		}
	}
	return Result{Name: "gh", Status: OK, Detail: strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])}
}

// checkEnvFile resolves and loads the .env exactly as a Stage does
// (config.ResolveEnvFile, then config.LoadDotEnv), and reports which file won and
// what it passed over — "but I set it!" is nearly always "in a file that was not
// read".
func checkEnvFile(p Probes) Result {
	loaded := config.ResolveEnvFile(p.Getenv, p.WorkingDir)
	candidates := config.EnvFileCandidates(p.Getenv, p.WorkingDir)
	if p.LoadEnv != nil {
		p.LoadEnv(loaded)
	}

	var b strings.Builder
	if loaded != "" {
		b.WriteString("loaded " + loaded)
	} else {
		b.WriteString("no .env file found; using exported variables only")
	}
	for _, c := range candidates {
		if c == loaded {
			continue
		}
		state := "not found"
		if fileExists(c) {
			state = "found, not read: an earlier file won"
		}
		fmt.Fprintf(&b, "\nskipped %s (%s)", c, state)
	}

	r := Result{Name: ".env", Status: OK, Detail: b.String()}
	hint := config.EnvFileHint(p.Getenv)
	switch {
	case loaded == "":
		r.Status = Warn
		r.Fix = "keep credentials in " + hint + " (copy .env.example), so every run reads the same ones"
	case config.EnvFileInsideProject(loaded, p.Getenv("PROJECT_PATH")):
		r.Status = Warn
		r.Detail += "\nit is inside the bind-mounted checkout, so every sandbox can read it"
		r.Fix = "move it to " + hint + ", or to <project>/.agent-harness/.env, the one in-repo path the sandbox masks"
	}
	return r
}

func checkProjectPath(p Probes) Result {
	path := p.Getenv("PROJECT_PATH")
	if problem := config.ProjectPathProblem(path); problem != "" {
		detail, fix, _ := strings.Cut(problem, "\n  Fix: ")
		return Result{Name: "PROJECT_PATH", Status: Fail, Detail: detail, Fix: strings.TrimSuffix(fix, ".")}
	}
	return Result{Name: "PROJECT_PATH", Status: OK, Detail: path}
}

// checkClaude checks the one credential that crosses into the sandbox. Exactly
// one should be set: Claude Code reads ANTHROPIC_API_KEY ahead of
// CLAUDE_CODE_OAUTH_TOKEN, so a subscription operator with a stray API key in
// their shell is billed per token without noticing.
func checkClaude(p Probes) Result {
	const name = "Claude credential"
	key := p.Getenv("ANTHROPIC_API_KEY")
	token := p.Getenv("CLAUDE_CODE_OAUTH_TOKEN")

	switch {
	case key == "" && token == "":
		return Result{Name: name, Status: Fail, Detail: "neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set",
			Fix: "set ANTHROPIC_API_KEY to a key from " + config.AnthropicKeyURL + ", or run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN"}
	case strings.HasPrefix(key, config.OAuthTokenPrefix):
		return Result{Name: name, Status: Fail, Detail: "ANTHROPIC_API_KEY holds a subscription token (" + config.OAuthTokenPrefix + "…), which Anthropic rejects as an API key",
			Fix: "move it to CLAUDE_CODE_OAUTH_TOKEN and unset ANTHROPIC_API_KEY"}
	case key == "":
		// A subscription token has no read-only endpoint to check it against, so
		// its shape is all doctor can vouch for.
		if !strings.HasPrefix(token, config.OAuthTokenPrefix) {
			return Result{Name: name, Status: Fail, Detail: "CLAUDE_CODE_OAUTH_TOKEN does not start with " + config.OAuthTokenPrefix + ", so it is not a subscription token",
				Fix: "run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN to the token it prints"}
		}
		return Result{Name: name, Status: OK, Detail: "CLAUDE_CODE_OAUTH_TOKEN set (a subscription token; its shape is checked, not its validity)"}
	}

	// Having both set needs no network call to spot, so --offline still warns.
	bothSet := Result{Name: name, Status: Warn,
		Detail: "both ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN are set; Claude Code uses ANTHROPIC_API_KEY, so sessions bill the API key, not the subscription",
		Fix:    "unset the one you do not mean to use"}
	if p.Offline {
		if token != "" {
			bothSet.Detail += "\nANTHROPIC_API_KEY not verified (--offline)"
			return bothSet
		}
		return Result{Name: name, Status: Skip, Detail: "ANTHROPIC_API_KEY set; not verified (--offline)"}
	}
	res, err := get(p, p.AnthropicAPI+"/v1/models", map[string]string{
		"x-api-key":         key,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return unreachable(name, "Anthropic", err)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("ANTHROPIC_API_KEY was rejected (HTTP %d)", res.StatusCode),
			Fix: "create a new key at " + config.AnthropicKeyURL}
	}
	if res.StatusCode >= 300 {
		return Result{Name: name, Status: Warn, Detail: fmt.Sprintf("could not verify ANTHROPIC_API_KEY: Anthropic answered HTTP %d", res.StatusCode)}
	}

	if token != "" {
		return bothSet
	}
	return Result{Name: name, Status: OK, Detail: "ANTHROPIC_API_KEY valid"}
}

// fineGrainedPrefix marks a fine-grained personal access token.
const fineGrainedPrefix = "github_pat_"

// checkGitHubToken checks GH_TOKEN is a live classic token with the `repo` scope,
// and warns without `workflow` — the two scopes the startup check
// (github.CheckToken) asks a replacement for. Only a classic token (or a gh OAuth
// token) answers with X-OAuth-Scopes; a fine-grained PAT has none, and it cannot
// read the check runs the post-PR CI watch depends on. `workflow` only warns:
// without it the harness works until a branch edits .github/workflows/, and then
// GitHub refuses the push.
func checkGitHubToken(p Probes) Result {
	const name = "GH_TOKEN"
	fix := "create a classic token with the repo and workflow scopes at " + config.GitHubTokenURL + " and set GH_TOKEN to it"
	token := p.Getenv("GH_TOKEN")
	switch {
	case token == "":
		return Result{Name: name, Status: Fail, Detail: "not set; the harness pushes branches and opens pull requests with it", Fix: fix}
	case strings.HasPrefix(token, fineGrainedPrefix):
		return fineGrained(fix)
	case p.Offline:
		return Result{Name: name, Status: Skip, Detail: "set; not verified (--offline)"}
	}

	res, err := get(p, p.GitHubAPI+"/user", map[string]string{
		"authorization": "Bearer " + token,
		"accept":        "application/vnd.github+json",
	})
	if err != nil {
		return unreachable(name, "GitHub", err)
	}
	if res.StatusCode == http.StatusUnauthorized {
		return Result{Name: name, Status: Fail, Detail: "rejected by GitHub (expired or revoked?)", Fix: fix}
	}
	if res.StatusCode >= 300 {
		return Result{Name: name, Status: Warn, Detail: fmt.Sprintf("could not verify: GitHub answered HTTP %d", res.StatusCode)}
	}
	header, classic := res.Header["X-Oauth-Scopes"]
	if !classic {
		return fineGrained(fix)
	}
	scopes := strings.Join(header, ",")
	has := map[string]bool{}
	for _, sc := range strings.Split(scopes, ",") {
		has[strings.TrimSpace(sc)] = true
	}
	switch {
	case !has["repo"]:
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("classic token without the repo scope (has: %s)", orNone(scopes)), Fix: fix}
	case !has["workflow"]:
		return Result{Name: name, Status: Warn,
			Detail: "classic token with repo but not workflow; GitHub will refuse a push that edits .github/workflows/", Fix: fix}
	}
	return Result{Name: name, Status: OK, Detail: "classic token with the repo and workflow scopes"}
}

func fineGrained(fix string) Result {
	return Result{Name: "GH_TOKEN", Status: Fail,
		Detail: "a fine-grained token; it cannot read check runs, so the post-PR CI watch is blind", Fix: fix}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

// unreachable is the verdict when a live check could not reach its API at all —
// a network problem, not a bad credential, so it says how to check without one.
func unreachable(name, service string, err error) Result {
	return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("could not reach %s: %v", service, err),
		Fix: "check the network, or re-run with --offline to skip the live checks"}
}

// get makes one read-only request and closes the body; callers read only the
// status and headers.
func get(p Probes, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := p.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return res, nil
}
