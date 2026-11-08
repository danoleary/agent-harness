package doctor

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// world is one fake host: the commands it answers, the remote APIs behind one
// httptest server, and the environment the operator set. Each test starts from a
// fully healthy world and breaks the one thing it is about, so a check's verdict
// is never an accident of some other check's prerequisite.
type world struct {
	t       *testing.T
	env     map[string]string
	cmds    map[string]cmdResult
	routes  map[string]http.HandlerFunc
	srv     *httptest.Server
	wd      string
	offline bool
	// sentry, when set, is a `[sentry]` table appended to the project config once
	// the server is up; a %s in it is the server's URL, for base_url.
	sentry string
}

type cmdResult struct {
	out string
	err error
}

func newWorld(t *testing.T) *world {
	t.Helper()
	project := t.TempDir()
	writeProject(t, project, githubConfig)
	w := &world{
		t:  t,
		wd: t.TempDir(),
		env: map[string]string{
			"PROJECT_PATH":      project,
			"ANTHROPIC_API_KEY": "sk-ant-api03-good",
			"GH_TOKEN":          "ghp_good",
			"HOME":              t.TempDir(),
		},
		cmds: map[string]cmdResult{
			"docker info":                         {out: "Server: Docker Engine"},
			"git --version":                       {out: "git version 2.47.1\n"},
			"gh --version":                        {out: "gh version 2.60.0 (2024-10-30)\n"},
			"docker image inspect example:latest": {out: "[{}]"},
		},
		routes: map[string]http.HandlerFunc{},
	}
	w.routes["GET /user"] = func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_good" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		rw.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = rw.Write([]byte(`{"login":"bot"}`))
	}
	w.routes["GET /v1/models"] = func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "sk-ant-api03-good" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = rw.Write([]byte(`{"data":[]}`))
	}
	write(t, filepath.Join(w.wd, ".env"), "")
	w.routes["GET /repos/acme/widgets"] = ok
	for _, label := range []string{"ready-for-agent", "in-progress", "agent-harness"} {
		w.routes["GET /repos/acme/widgets/labels/"+label] = ok
	}
	return w
}

func ok(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write([]byte(`{}`)) }

func (w *world) run() []Result {
	w.t.Helper()
	mux := http.NewServeMux()
	for route, h := range w.routes {
		mux.HandleFunc(route, h)
	}
	w.srv = httptest.NewServer(mux)
	w.t.Cleanup(w.srv.Close)
	// The Jira site is the operator's own, so its URL comes from the environment
	// rather than a fixed API base. A non-jira world never reads it.
	w.env["JIRA_BASE_URL"] = w.srv.URL
	if w.sentry != "" {
		writeProject(w.t, w.env["PROJECT_PATH"], githubConfig+strings.ReplaceAll(w.sentry, "%s", w.srv.URL))
	}
	return Run(Probes{
		Getenv:     func(k string) string { return w.env[k] },
		WorkingDir: w.wd,
		Run: func(name string, args ...string) ([]byte, error) {
			line := strings.Join(append([]string{name}, args...), " ")
			r, found := w.cmds[line]
			if !found {
				return []byte(name + ": command not found"), errors.New("exec: not found")
			}
			return []byte(r.out), r.err
		},
		HTTP:         w.srv.Client(),
		GitHubAPI:    w.srv.URL,
		AnthropicAPI: w.srv.URL,
		LinearAPI:    w.srv.URL + "/graphql",
		Offline:      w.offline,
	})
}

func find(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q check in %+v", name, results)
	return Result{}
}

const githubConfig = `
image = "example:latest"
branch_prefix = "feat"

[tracker]
kind = "github"
repo = "acme/widgets"
findings_label_id = "agent-harness"
ready_label = "ready-for-agent"
in_progress_label = "in-progress"

[[gates]]
name = "test"
command = "go test ./..."
`

func writeProject(t *testing.T, project, config string) {
	t.Helper()
	dir := filepath.Join(project, ".agent-harness", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(project, ".agent-harness", "config.toml"), config)
	for _, name := range []string{"implement", "review", "retro"} {
		write(t, filepath.Join(dir, name+".md"), "Invoke /"+name+".\n")
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The point of doctor is that a healthy setup says so in one pass, every line
// green — and that nothing in a healthy setup is reported as a problem.
func TestHealthySetupPassesEveryCheck(t *testing.T) {
	results := newWorld(t).run()
	for _, r := range results {
		if r.Status != OK {
			t.Errorf("%s: %s %q (fix %q), want ok", r.Name, r.Status, r.Detail, r.Fix)
		}
	}
	if Failed(results) {
		t.Error("Failed() = true for a healthy setup")
	}
}

// A newcomer's machine is usually wrong in several ways at once; doctor must
// report all of them in one run, each with its own fix, rather than stopping at
// the first.
func TestReportsEveryProblemAtOnceEachWithAFix(t *testing.T) {
	w := newWorld(t)
	delete(w.cmds, "docker info")
	w.cmds["git --version"] = cmdResult{out: "git version 2.39.5 (Apple Git-154)\n"}
	delete(w.cmds, "gh --version")
	delete(w.env, "GH_TOKEN")
	results := w.run()

	for _, name := range []string{"docker", "git", "gh", "GH_TOKEN"} {
		r := find(t, results, name)
		if r.Status != Fail {
			t.Errorf("%s: %s, want fail", name, r.Status)
		}
		if r.Fix == "" {
			t.Errorf("%s: failing line has no fix", name)
		}
	}
	if !Failed(results) {
		t.Error("Failed() = false with failing checks; doctor must exit non-zero")
	}
}

func TestGitVersionFloor(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Status
	}{
		{"git version 2.45.0\n", OK},
		{"git version 2.50.1 (Apple Git-155)\n", OK},
		{"git version 3.0.0\n", OK},
		{"git version 2.44.2\n", Fail},
		{"git version 2.39.5 (Apple Git-154)\n", Fail},
	} {
		w := newWorld(t)
		w.cmds["git --version"] = cmdResult{out: tc.out}
		if got := find(t, w.run(), "git"); got.Status != tc.want {
			t.Errorf("%q: %s, want %s", tc.out, got.Status, tc.want)
		}
	}
}

// Which .env was read is the question behind most "but I set it!" reports, so
// doctor names the file it loaded and every candidate it passed over.
func TestEnvFileNamesTheLoadedFileAndTheSkippedCandidates(t *testing.T) {
	w := newWorld(t)
	local := filepath.Join(w.wd, ".env")
	write(t, local, "")
	global := filepath.Join(w.env["HOME"], ".config", "agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, global, "")

	r := find(t, w.run(), ".env")
	if r.Status != OK {
		t.Fatalf(".env: %s %q, want ok", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "loaded "+local) {
		t.Errorf("detail %q does not name the loaded file %s", r.Detail, local)
	}
	if !strings.Contains(r.Detail, global+" (found, not read") {
		t.Errorf("detail %q does not say %s was shadowed", r.Detail, global)
	}
	inRepo := filepath.Join(w.env["PROJECT_PATH"], ".agent-harness", ".env")
	if !strings.Contains(r.Detail, inRepo+" (not found)") {
		t.Errorf("detail %q does not list the missing candidate %s", r.Detail, inRepo)
	}
}

func TestNoEnvFileIsAWarningNotAFailure(t *testing.T) {
	w := newWorld(t)
	if err := os.Remove(filepath.Join(w.wd, ".env")); err != nil {
		t.Fatal(err)
	}
	r := find(t, w.run(), ".env")
	if r.Status != Warn {
		t.Errorf(".env: %s, want warn when every variable is exported", r.Status)
	}
}

// Claude Code prefers ANTHROPIC_API_KEY when both are set, so a subscription
// operator with a stray API key in their shell is silently billed per token.
func TestWarnsWhenBothClaudeCredentialsAreSet(t *testing.T) {
	w := newWorld(t)
	w.env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-sub"
	r := find(t, w.run(), "Claude credential")
	if r.Status != Warn {
		t.Fatalf("Claude credential: %s, want warn", r.Status)
	}
	if !strings.Contains(r.Fix, "unset") {
		t.Errorf("fix %q does not say which one to unset", r.Fix)
	}
}

func TestClaudeCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Status
	}{
		{"none", map[string]string{"ANTHROPIC_API_KEY": ""}, Fail},
		{"rejected key", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-revoked"}, Fail},
		{"subscription token as api key", map[string]string{"ANTHROPIC_API_KEY": "sk-ant-oat01-x"}, Fail},
		{"subscription token", map[string]string{"ANTHROPIC_API_KEY": "", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x"}, OK},
		{"malformed subscription token", map[string]string{"ANTHROPIC_API_KEY": "", "CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-api03-x"}, Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			for k, v := range tc.env {
				w.env[k] = v
			}
			r := find(t, w.run(), "Claude credential")
			if r.Status != tc.want {
				t.Errorf("%s %q, want %s", r.Status, r.Detail, tc.want)
			}
			if r.Status == Fail && r.Fix == "" {
				t.Error("failing line has no fix")
			}
		})
	}
}

// GH_TOKEN must be a classic token with `repo`: the scope list only exists on a
// classic token's X-OAuth-Scopes header, and a fine-grained PAT cannot read the
// check runs the post-PR CI watch depends on. `workflow` is the scope the startup
// check (github.CheckToken) also asks for: without it GitHub refuses a push that
// edits .github/workflows/, so its absence warns rather than passing silently (#119).
func TestGitHubTokenScopes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes *string
		status int
		want   Status
		detail string
	}{
		{"classic with repo and workflow", ptr("repo, workflow"), 200, OK, "workflow"},
		{"classic without workflow", ptr("repo, read:org"), 200, Warn, "workflow"},
		{"classic without repo", ptr("read:org, gist"), 200, Fail, "repo"},
		{"fine-grained", nil, 200, Fail, "fine-grained"},
		{"rejected", ptr("repo"), 401, Fail, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.routes["GET /user"] = func(rw http.ResponseWriter, _ *http.Request) {
				if tc.scopes != nil {
					rw.Header().Set("X-OAuth-Scopes", *tc.scopes)
				}
				rw.WriteHeader(tc.status)
				_, _ = rw.Write([]byte(`{"message":"Bad credentials"}`))
			}
			r := find(t, w.run(), "GH_TOKEN")
			if r.Status != tc.want {
				t.Errorf("%s %q, want %s", r.Status, r.Detail, tc.want)
			}
			if !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("detail %q does not mention %q", r.Detail, tc.detail)
			}
			if r.Status != OK && !strings.Contains(r.Fix, "github.com/settings/tokens") {
				t.Errorf("fix %q does not link the classic-token page", r.Fix)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// --offline is for a plane or a CI box: it must make no network call at all, and
// say plainly which lines it did not verify rather than passing them.
// Both credentials being set is visible without a network call, so --offline
// must not hide it behind a skip.
func TestWarnsWhenBothClaudeCredentialsAreSetOffline(t *testing.T) {
	w := newWorld(t)
	w.offline = true
	w.env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-sub"
	r := find(t, w.run(), "Claude credential")
	if r.Status != Warn || !strings.Contains(r.Detail, "--offline") {
		t.Errorf("Claude credential offline: %s %q, want a warn that says the key was not verified", r.Status, r.Detail)
	}
}

// A label lookup GitHub did not answer (a rate limit, an outage) is neither
// present nor missing, so it must not pass.
func TestGitHubTrackerLabelLookupErrorIsNotAPass(t *testing.T) {
	w := newWorld(t)
	w.routes["GET /repos/acme/widgets/labels/in-progress"] = func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
	}
	if r := find(t, w.run(), "tracker"); r.Status == OK {
		t.Errorf("tracker: ok %q, want a non-pass when a label lookup errors", r.Detail)
	}
}

func TestOfflineMakesNoNetworkCalls(t *testing.T) {
	w := newWorld(t)
	w.offline = true
	w.cmds["docker image inspect example:latest"] = cmdResult{err: errors.New("no such image")}
	w.routes["/"] = func(rw http.ResponseWriter, r *http.Request) {
		t.Errorf("offline doctor called %s %s", r.Method, r.URL.Path)
	}
	for k := range w.routes {
		if k != "/" {
			w.routes[k] = w.routes["/"]
		}
	}
	results := w.run()
	for _, name := range []string{"GH_TOKEN", "Claude credential", "tracker"} {
		r := find(t, results, name)
		if r.Status != Skip {
			t.Errorf("%s: %s, want skip offline", name, r.Status)
		}
	}
	if r := find(t, results, "sandbox image"); r.Status != Skip {
		t.Errorf("sandbox image: %s, want skip — pullability is a registry call", r.Status)
	}
	if Failed(results) {
		t.Error("a skipped check must not fail the run")
	}
}

func TestProjectConfigAndPromptsLoad(t *testing.T) {
	w := newWorld(t)
	if err := os.Remove(filepath.Join(w.env["PROJECT_PATH"], ".agent-harness", "prompts", "review.md")); err != nil {
		t.Fatal(err)
	}
	results := w.run()
	if r := find(t, results, "config.toml"); r.Status != OK {
		t.Errorf("config.toml: %s %q, want ok", r.Status, r.Detail)
	}
	r := find(t, results, "prompt bodies")
	if r.Status != Fail || !strings.Contains(r.Detail, "review.md") {
		t.Errorf("prompt bodies: %s %q, want a failure naming review.md", r.Status, r.Detail)
	}
}

// Without a usable PROJECT_PATH there is no config to read, and the checks that
// need it say what they are waiting on rather than piling up as failures of their
// own.
func TestChecksThatNeedTheProjectSkipWithoutIt(t *testing.T) {
	w := newWorld(t)
	w.env["PROJECT_PATH"] = filepath.Join(t.TempDir(), "nope")
	results := w.run()
	if r := find(t, results, "PROJECT_PATH"); r.Status != Fail || r.Fix == "" {
		t.Errorf("PROJECT_PATH: %s fix %q, want a failure with a fix", r.Status, r.Fix)
	}
	for _, name := range []string{"config.toml", "prompt bodies", "tracker", "sandbox image"} {
		r := find(t, results, name)
		if r.Status != Skip || !strings.Contains(r.Detail, "PROJECT_PATH") {
			t.Errorf("%s: %s %q, want a skip naming PROJECT_PATH", name, r.Status, r.Detail)
		}
	}
}

func TestGitHubTrackerChecksTheRepoAndLabels(t *testing.T) {
	w := newWorld(t)
	delete(w.routes, "GET /repos/acme/widgets/labels/in-progress")
	r := find(t, w.run(), "tracker")
	if r.Status != Fail || !strings.Contains(r.Detail, "in-progress") {
		t.Errorf("tracker: %s %q, want a failure naming the missing label", r.Status, r.Detail)
	}
	if !strings.Contains(r.Fix, "in-progress") {
		t.Errorf("fix %q does not say which label to create", r.Fix)
	}

	w = newWorld(t)
	delete(w.routes, "GET /repos/acme/widgets")
	if r := find(t, w.run(), "tracker"); r.Status != Fail || !strings.Contains(r.Detail, "acme/widgets") {
		t.Errorf("tracker: %s %q, want a failure naming the repo", r.Status, r.Detail)
	}
}

const linearConfig = `
image = "example:latest"
branch_prefix = "feat"

[tracker]
kind = "linear"
team_key = "BEH"
findings_label_id = "00000000-0000-0000-0000-000000000000"
ready_label = "ready-for-agent"
blocked_label = "blocked"

[[gates]]
name = "test"
command = "go test ./..."
`

func TestLinearTrackerChecksKeyTeamAndLabels(t *testing.T) {
	linear := func(teams, labels string) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "lin_api_good" {
				_, _ = rw.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
				return
			}
			_, _ = rw.Write([]byte(`{"data":{"viewer":{"id":"u"},"teams":{"nodes":[` + teams +
				`]},"issueLabels":{"nodes":[` + labels + `]}}}`))
		}
	}
	for _, tc := range []struct {
		name, key, teams, labels string
		want                     Status
		detail                   string
	}{
		{"healthy", "lin_api_good", `{"id":"t"}`, `{"name":"ready-for-agent"},{"name":"blocked"}`, OK, "BEH"},
		{"rejected key", "lin_api_bad", `{"id":"t"}`, ``, Fail, "LINEAR_API_KEY"},
		{"no team", "lin_api_good", ``, `{"name":"ready-for-agent"},{"name":"blocked"}`, Fail, "BEH"},
		{"no label", "lin_api_good", `{"id":"t"}`, `{"name":"ready-for-agent"}`, Fail, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			writeProject(t, w.env["PROJECT_PATH"], linearConfig)
			w.env["LINEAR_API_KEY"] = tc.key
			w.routes["POST /graphql"] = linear(tc.teams, tc.labels)
			r := find(t, w.run(), "tracker")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("%s %q, want %s mentioning %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}

	w := newWorld(t)
	writeProject(t, w.env["PROJECT_PATH"], linearConfig)
	r := find(t, w.run(), "tracker")
	if r.Status != Fail || !strings.Contains(r.Fix, "linear.app/settings") {
		t.Errorf("unset LINEAR_API_KEY: %s fix %q, want a failure linking the key page", r.Status, r.Fix)
	}
}

const jiraConfig = `
image = "example:latest"
branch_prefix = "feat"

[tracker]
kind = "jira"
project_key = "PROJ"
findings_label_id = "agent-harness"
ready_jql = "project = PROJ AND labels = ready"
in_progress_jql = "project = PROJ AND status = 'In Progress'"
in_progress_transition = "Start"
todo_transition = "Stop"
cancel_transition = "Cancel"

[[gates]]
name = "test"
command = "go test ./..."
`

func TestJiraTrackerChecksCredentialsAndProject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project bool
		me      int
		want    Status
		detail  string
	}{
		{"healthy", true, 200, OK, "PROJ"},
		{"rejected", true, 401, Fail, "JIRA"},
		{"no project", false, 200, Fail, "PROJ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			writeProject(t, w.env["PROJECT_PATH"], jiraConfig)
			w.env["JIRA_EMAIL"] = "bot@acme.test"
			w.env["JIRA_API_TOKEN"] = "tok"
			w.routes["GET /rest/api/3/myself"] = func(rw http.ResponseWriter, _ *http.Request) {
				rw.WriteHeader(tc.me)
			}
			if tc.project {
				w.routes["GET /rest/api/3/project/PROJ"] = ok
			}
			r := find(t, w.run(), "tracker")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("%s %q, want %s mentioning %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}

func TestSandboxImage(t *testing.T) {
	t.Run("pullable", func(t *testing.T) {
		w := newWorld(t)
		w.cmds["docker image inspect example:latest"] = cmdResult{err: errors.New("no such image")}
		w.cmds["docker manifest inspect example:latest"] = cmdResult{out: "{}"}
		if r := find(t, w.run(), "sandbox image"); r.Status != OK || !strings.Contains(r.Detail, "pull") {
			t.Errorf("%s %q, want ok, pullable", r.Status, r.Detail)
		}
	})
	t.Run("neither present nor pullable", func(t *testing.T) {
		w := newWorld(t)
		w.cmds["docker image inspect example:latest"] = cmdResult{err: errors.New("no such image")}
		r := find(t, w.run(), "sandbox image")
		if r.Status != Fail || !strings.Contains(r.Fix, "docker pull example:latest") {
			t.Errorf("%s fix %q, want a failure suggesting the pull", r.Status, r.Fix)
		}
	})
	t.Run("buildable from the Consumer Dockerfile", func(t *testing.T) {
		w := newWorld(t)
		writeProject(t, w.env["PROJECT_PATH"], "dockerfile = \".agent-harness/Dockerfile\"\n"+githubConfig)
		write(t, filepath.Join(w.env["PROJECT_PATH"], ".agent-harness", "Dockerfile"), "FROM scratch\n")
		w.cmds["docker image inspect example:latest"] = cmdResult{err: errors.New("no such image")}
		if r := find(t, w.run(), "sandbox image"); r.Status != OK || !strings.Contains(r.Detail, "build") {
			t.Errorf("%s %q, want ok, built on first run", r.Status, r.Detail)
		}
	})
	t.Run("declared Dockerfile is missing", func(t *testing.T) {
		w := newWorld(t)
		writeProject(t, w.env["PROJECT_PATH"], "dockerfile = \".agent-harness/Dockerfile\"\n"+githubConfig)
		w.cmds["docker image inspect example:latest"] = cmdResult{err: errors.New("no such image")}
		if r := find(t, w.run(), "sandbox image"); r.Status != Fail {
			t.Errorf("%s %q, want fail", r.Status, r.Detail)
		}
	})
	t.Run("no daemon", func(t *testing.T) {
		w := newWorld(t)
		delete(w.cmds, "docker info")
		if r := find(t, w.run(), "sandbox image"); r.Status != Skip {
			t.Errorf("%s, want skip — nothing to ask without a daemon", r.Status)
		}
	})
}

// The rendered checklist is the whole interface: one line per check, its fix
// indented beneath it, and a closing tally.
func TestRenderPrintsAChecklistWithFixes(t *testing.T) {
	var b bytes.Buffer
	Render(&b, []Result{
		{Name: "docker", Status: OK, Detail: "daemon reachable"},
		{Name: "git", Status: Fail, Detail: "2.39.5 is older than 2.45", Fix: "upgrade git"},
		{Name: ".env", Status: Warn, Detail: "no file"},
		{Name: "tracker", Status: Skip, Detail: "--offline"},
	})
	got := b.String()
	for _, want := range []string{
		"[✓] docker: daemon reachable\n",
		"[✗] git: 2.39.5 is older than 2.45\n      fix: upgrade git\n",
		"[!] .env: no file\n",
		"[-] tracker: --offline\n",
		"1 problem, 1 warning",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// #211: with a `[sentry]` table, doctor checks the token is set and that Sentry
// accepts it with one cheap read of the org. Without one there is no Sentry line.
func TestSentryChecksTheTokenWhenConfigured(t *testing.T) {
	const table = "\n[sentry]\norg = \"acme\"\nprojects = [\"HERD\"]\nbase_url = \"%s\"\n"
	good := func(w *world) {
		w.routes["GET /api/0/organizations/acme/{$}"] = func(rw http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sntrys_good" {
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = rw.Write([]byte(`{"slug":"acme"}`))
		}
	}

	off := newWorld(t).run()
	for _, r := range off {
		if r.Name == "Sentry" {
			t.Errorf("a Sentry line without [sentry]: %+v", r)
		}
	}

	for _, tc := range []struct {
		name    string
		token   string
		offline bool
		want    Status
		detail  string
	}{
		{"accepted", "sntrys_good", false, OK, "acme"},
		{"unset", "", false, Fail, "SENTRY_AUTH_TOKEN is not set"},
		{"rejected", "sntrys_bad", false, Fail, "HTTP 401"},
		{"offline", "sntrys_good", true, Skip, "--offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			good(w)
			w.sentry = table
			w.offline = tc.offline
			if tc.token != "" {
				w.env["SENTRY_AUTH_TOKEN"] = tc.token
			}
			r := find(t, w.run(), "Sentry")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("Sentry = %s %q, want %s naming %q", r.Status, r.Detail, tc.want, tc.detail)
			}
			if tc.want == Fail && !strings.Contains(r.Fix, "event:read") {
				t.Errorf("fix %q does not name the scopes", r.Fix)
			}
		})
	}
}

// securityLoop declares the security-alerts loop (ADR-0040) in a project config.
const securityLoop = `
[[loops]]
name = "security-alerts"
source = { kind = "github_security_alerts" }
steps = [
  { use = "new_branch" },
  { use = "fix_alerts" },
  { use = "gates" },
  { use = "open_pr" },
]
`

// #205: a declared github_security_alerts loop reads the repository's Dependabot
// alerts with GH_TOKEN, so doctor makes that one read (per_page=1) against the
// checkout's origin and says why it fails: the token's scope or permission, or
// alerts turned off on the repository. Without the loop there is no line.
func TestDependabotAlertsCheckedWhenTheSecurityLoopIsDeclared(t *testing.T) {
	const name = "Dependabot alerts"
	for _, r := range newWorld(t).run() {
		if r.Name == name {
			t.Errorf("a %s line without a security loop: %+v", name, r)
		}
	}

	forbidden := func(scopes, message string) http.HandlerFunc {
		return func(rw http.ResponseWriter, _ *http.Request) {
			if scopes != "" {
				rw.Header().Set("X-OAuth-Scopes", scopes)
			}
			rw.WriteHeader(http.StatusForbidden)
			_, _ = rw.Write([]byte(`{"message":"` + message + `"}`))
		}
	}
	for _, tc := range []struct {
		name    string
		remote  string
		route   http.HandlerFunc
		token   string
		offline bool
		want    Status
		detail  string
		fix     string
	}{
		{name: "readable", route: ok, want: OK, detail: "acme/widgets"},
		{name: "classic token without security_events",
			route: forbidden("public_repo, workflow", "Resource not accessible by personal access token"),
			want:  Fail, detail: "public_repo, workflow", fix: "security_events"},
		{name: "fine-grained token without Dependabot alerts read",
			route: forbidden("", "Resource not accessible by personal access token"),
			want:  Fail, detail: "fine-grained", fix: "Dependabot alerts: Read"},
		{name: "alerts disabled on the repo",
			route: forbidden("repo, workflow", "Dependabot alerts are disabled for this repository."),
			want:  Fail, detail: "disabled", fix: "Settings"},
		{name: "repo not visible", route: func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusNotFound) },
			want: Fail, detail: "HTTP 404", fix: "origin"},
		{name: "origin is not on GitHub", remote: "git@gitlab.com:acme/widgets.git", route: ok, want: Fail, detail: "gitlab.com", fix: "origin"},
		{name: "offline", route: ok, offline: true, want: Skip, detail: "--offline"},
		{name: "no token", route: ok, token: "-", want: Skip, detail: "GH_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			project := w.env["PROJECT_PATH"]
			writeProject(t, project, githubConfig+securityLoop)
			remote := "https://github.com/acme/widgets.git"
			if tc.remote != "" {
				remote = tc.remote
			}
			w.cmds["git -C "+project+" remote get-url origin"] = cmdResult{out: remote + "\n"}
			w.routes["GET /repos/acme/widgets/dependabot/alerts"] = func(rw http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("per_page"); got != "1" {
					t.Errorf("per_page = %q, want 1: the check needs one alert at most", got)
				}
				tc.route(rw, r)
			}
			w.offline = tc.offline
			if tc.token == "-" {
				delete(w.env, "GH_TOKEN")
			}
			r := find(t, w.run(), name)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("%s = %s %q, want %s naming %q", name, r.Status, r.Detail, tc.want, tc.detail)
			}
			if !strings.Contains(r.Fix, tc.fix) {
				t.Errorf("fix %q does not say %q", r.Fix, tc.fix)
			}
		})
	}
}
