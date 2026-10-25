package scaffold

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
)

// checkout makes a temp Consumer checkout holding the named files.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func origin(url string) func(string) (string, error) {
	return func(string) (string, error) { return url, nil }
}

func noOrigin(string) (string, error) { return "", errors.New("no such remote 'origin'") }

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDetectToolchain(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"go module", map[string]string{"go.mod": "module x\n"}, "go"},
		{"pnpm lockfile", map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""}, "pnpm"},
		{"pnpm workspace", map[string]string{"pnpm-workspace.yaml": ""}, "pnpm"},
		{"dotnet solution", map[string]string{"App.sln": ""}, "dotnet"},
		{"dotnet project", map[string]string{"App.csproj": ""}, "dotnet"},
		{"python pyproject", map[string]string{"pyproject.toml": ""}, "python"},
		{"python requirements", map[string]string{"requirements.txt": ""}, "python"},
		{"nothing recognisable", map[string]string{"README.md": ""}, ""},
		// An npm or yarn project is not one of the day-one toolchains: guessing
		// pnpm for it would write a post_create that fails on every worktree.
		{"npm is not pnpm", map[string]string{"package.json": "{}", "package-lock.json": ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if tcn, ok := Detect(checkout(t, tc.files)); ok {
				got = tcn.Name
			}
			if got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubRepoFromRemote(t *testing.T) {
	cases := []struct{ url, want string }{
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"https://x-access-token:abc@github.com/acme/widgets.git", "acme/widgets"},
		{"ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"git@gitlab.com:acme/widgets.git", ""},
		{"https://github.com/acme", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := GitHubRepo(tc.url); got != tc.want {
			t.Errorf("GitHubRepo(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// The scaffold is only worth anything if the harness can run on it: every
// toolchain's output must load through the real config and prompt loaders, with
// no key the loader does not read.
func TestInitYesScaffoldsALoadableConsumer(t *testing.T) {
	cases := []struct {
		toolchain string
		files     map[string]string
		gates     []string
		strip     []string
	}{
		{"go", map[string]string{"go.mod": "module x\n\ngo 1.26.2\n"}, []string{"go test ./...", "go vet ./..."}, nil},
		{"dotnet", map[string]string{"App.sln": ""}, []string{"dotnet test", "dotnet format --verify-no-changes"}, []string{"obj", "bin"}},
		{"pnpm", map[string]string{"pnpm-lock.yaml": "", "package.json": `{"scripts":{"test":"vitest","typecheck":"tsc"}}`}, []string{"pnpm typecheck", "pnpm test"}, []string{"node_modules"}},
		{"python", map[string]string{"pyproject.toml": "", "uv.lock": ""}, []string{"uv run pytest"}, []string{".venv"}},
	}
	for _, tc := range cases {
		t.Run(tc.toolchain, func(t *testing.T) {
			dir := checkout(t, tc.files)
			var out bytes.Buffer
			err := Run(Options{Dir: dir, Yes: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &out})
			if err != nil {
				t.Fatalf("Run: %v\n%s", err, out.String())
			}

			pc, err := config.LoadProject(dir)
			if err != nil {
				t.Fatalf("scaffolded config does not load: %v", err)
			}
			if len(pc.Warnings) != 0 {
				t.Errorf("scaffolded config has keys the harness does not read: %q", pc.Warnings)
			}
			if pc.Tracker.Kind != "github" || pc.Tracker.Repo != "acme/widgets" {
				t.Errorf("tracker = %+v, want github acme/widgets from the origin remote", pc.Tracker)
			}
			if pc.Dockerfile != ".agent-harness/Dockerfile" || pc.Image == "" {
				t.Errorf("image source = %q / %q, want the scaffolded Dockerfile and a local tag", pc.Dockerfile, pc.Image)
			}
			if pc.PostCreate == "" {
				t.Error("scaffolded config has no post_create")
			}
			var gates []string
			for _, g := range pc.Gates {
				gates = append(gates, g.Command)
			}
			if strings.Join(gates, "|") != strings.Join(tc.gates, "|") {
				t.Errorf("gates = %q, want %q", gates, tc.gates)
			}
			if strings.Join(pc.HandoffStripPaths, "|") != strings.Join(tc.strip, "|") {
				t.Errorf("handoff_strip_paths = %q, want %q", pc.HandoffStripPaths, tc.strip)
			}

			df := read(t, filepath.Join(dir, ".agent-harness", "Dockerfile"))
			if !regexp.MustCompile(`(?m)^FROM ghcr\.io/danoleary/agent-harness-base:\d+\.\d+\.\d+$`).MatchString(df) {
				t.Errorf("Dockerfile does not FROM a pinned base tag:\n%s", df)
			}

			if _, err := config.LoadPrompts(dir); err != nil {
				t.Fatalf("scaffolded prompts do not load: %v", err)
			}
			for _, name := range []string{"implement", "review", "retro"} {
				body := read(t, filepath.Join(dir, ".agent-harness", "prompts", name+".md"))
				if !strings.Contains(body, PlaceholderMarker) {
					t.Errorf("%s.md does not mark its placeholder skill with %q:\n%s", name, PlaceholderMarker, body)
				}
			}

			if !strings.Contains(out.String(), NextCommand) {
				t.Errorf("output does not end by naming the next command %q:\n%s", NextCommand, out.String())
			}
		})
	}
}

// .env lands at the one in-repo path the harness masks in every sandbox, is
// readable only by its owner, names this checkout, and is gitignored.
func TestInitWritesAMaskedGitignoredEnvFile(t *testing.T) {
	dir := checkout(t, map[string]string{"go.mod": "module x\n", ".gitignore": "/bin/\n"})
	if err := Run(Options{Dir: dir, Yes: true, OriginURL: origin("https://github.com/acme/widgets"), In: strings.NewReader(""), Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	env := config.InRepoEnvFile(dir)
	info, err := os.Stat(env)
	if err != nil {
		t.Fatalf("no .env at the masked path %s: %v", env, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env mode = %o, want 600", perm)
	}
	body := read(t, env)
	if !strings.Contains(body, "\nPROJECT_PATH="+dir+"\n") {
		t.Errorf(".env does not set PROJECT_PATH to the checkout %s", dir)
	}
	if !strings.Contains(body, "GH_TOKEN=") || !strings.Contains(body, "CLAUDE_CODE_OAUTH_TOKEN=") {
		t.Error(".env is not the credential template")
	}

	gi := read(t, filepath.Join(dir, ".gitignore"))
	if !strings.HasPrefix(gi, "/bin/\n") {
		t.Errorf(".gitignore lost its existing content:\n%s", gi)
	}
	if !regexp.MustCompile(`(?m)^\.agent-harness/\.env$`).MatchString(gi) {
		t.Errorf(".gitignore does not ignore .agent-harness/.env:\n%s", gi)
	}
}

// The credential template ships inside the binary, so it must not drift from
// the .env.example the README and the release archive document.
func TestEnvTemplateIsTheRepoTemplate(t *testing.T) {
	want := read(t, filepath.Join("..", "..", ".env.example"))
	if envTemplate != want {
		t.Error("internal/scaffold/templates/env.example has drifted from .env.example; copy it across")
	}
}

// The base pin is bumped by scripts/release.sh alongside the harness's own
// Dockerfile; the two must name the same release.
func TestDockerfilePinMatchesTheHarnessOwn(t *testing.T) {
	pin := regexp.MustCompile(`(?m)^FROM (ghcr\.io/\S+/agent-harness-base:\S+)`)
	own := pin.FindStringSubmatch(read(t, filepath.Join("..", "..", ".agent-harness", "Dockerfile")))
	ours := pin.FindStringSubmatch(dockerfileTemplate)
	if own == nil || ours == nil || own[1] != ours[1] {
		t.Errorf("scaffold pins %v, the harness's own Dockerfile pins %v", ours, own)
	}
}

func TestInitNeverOverwritesWithoutForce(t *testing.T) {
	dir := checkout(t, map[string]string{
		"go.mod":                              "module x\n",
		".agent-harness/config.toml":          "# mine\n",
		".agent-harness/prompts/implement.md": "/my-skill\n",
	})
	var out bytes.Buffer
	if err := Run(Options{Dir: dir, Yes: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &out}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, ".agent-harness", "config.toml")); got != "# mine\n" {
		t.Errorf("config.toml overwritten without --force:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, ".agent-harness", "prompts", "implement.md")); got != "/my-skill\n" {
		t.Errorf("implement.md overwritten without --force:\n%s", got)
	}
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("output does not say how to overwrite the kept files:\n%s", out.String())
	}
	// The files that were missing are still written.
	if _, err := os.Stat(filepath.Join(dir, ".agent-harness", "prompts", "review.md")); err != nil {
		t.Errorf("a missing file was not written alongside the kept ones: %v", err)
	}

	if err := Run(Options{Dir: dir, Yes: true, Force: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadProject(dir); err != nil {
		t.Errorf("--force did not replace the hand-written config: %v", err)
	}
}

// Running init twice must not stack duplicate .gitignore lines.
func TestInitGitignoreIsIdempotent(t *testing.T) {
	dir := checkout(t, map[string]string{"go.mod": "module x\n"})
	for i := 0; i < 2; i++ {
		if err := Run(Options{Dir: dir, Yes: true, Force: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &bytes.Buffer{}}); err != nil {
			t.Fatal(err)
		}
	}
	gi := read(t, filepath.Join(dir, ".gitignore"))
	if n := strings.Count(gi, ".agent-harness/.env\n"); n != 1 {
		t.Errorf(".agent-harness/.env appears %d times:\n%s", n, gi)
	}
}

// Interactively, each answer overrides the detected default and an empty line
// accepts it.
func TestInitPromptsForToolchainAndRepo(t *testing.T) {
	dir := checkout(t, map[string]string{"go.mod": "module x\n", "requirements.txt": ""})
	var out bytes.Buffer
	err := Run(Options{Dir: dir, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader("python\nacme/other\n"), Out: &out})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	pc, err := config.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Tracker.Repo != "acme/other" {
		t.Errorf("repo = %q, want the typed answer", pc.Tracker.Repo)
	}
	if pc.Gates[0].Command != "uv run pytest" {
		t.Errorf("gates = %+v, want the python toolchain that was typed", pc.Gates)
	}
	if !strings.Contains(out.String(), "[go]") || !strings.Contains(out.String(), "[acme/widgets]") {
		t.Errorf("prompts do not show the detected defaults:\n%s", out.String())
	}

	dir = checkout(t, map[string]string{"go.mod": "module x\n"})
	if err := Run(Options{Dir: dir, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader("\n\n"), Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if pc, err := config.LoadProject(dir); err != nil || pc.Tracker.Repo != "acme/widgets" || pc.Gates[0].Command != "go test ./..." {
		t.Errorf("empty answers did not accept the defaults: %+v, %v", pc, err)
	}
}

func TestInitRefusesWhatItCannotDefault(t *testing.T) {
	t.Run("no toolchain under --yes", func(t *testing.T) {
		dir := checkout(t, map[string]string{"README.md": ""})
		err := Run(Options{Dir: dir, Yes: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &bytes.Buffer{}})
		if err == nil || !strings.Contains(err.Error(), "--toolchain") {
			t.Errorf("err = %v, want one naming --toolchain", err)
		}
	})
	t.Run("no GitHub remote under --yes", func(t *testing.T) {
		dir := checkout(t, map[string]string{"go.mod": "module x\n"})
		err := Run(Options{Dir: dir, Yes: true, OriginURL: noOrigin, In: strings.NewReader(""), Out: &bytes.Buffer{}})
		if err == nil || !strings.Contains(err.Error(), "--repo") {
			t.Errorf("err = %v, want one naming --repo", err)
		}
	})
	t.Run("an unknown toolchain", func(t *testing.T) {
		dir := checkout(t, map[string]string{"go.mod": "module x\n"})
		err := Run(Options{Dir: dir, Yes: true, Toolchain: "cobol", OriginURL: noOrigin, Repo: "acme/widgets", In: strings.NewReader(""), Out: &bytes.Buffer{}})
		if err == nil || !strings.Contains(err.Error(), "cobol") {
			t.Errorf("err = %v, want one naming the unknown toolchain", err)
		}
	})
	t.Run("a malformed repo", func(t *testing.T) {
		dir := checkout(t, map[string]string{"go.mod": "module x\n"})
		err := Run(Options{Dir: dir, Yes: true, Repo: "widgets", OriginURL: noOrigin, In: strings.NewReader(""), Out: &bytes.Buffer{}})
		if err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("err = %v, want one naming the owner/name shape", err)
		}
	})
	t.Run("nothing written on refusal", func(t *testing.T) {
		dir := checkout(t, map[string]string{"README.md": ""})
		_ = Run(Options{Dir: dir, Yes: true, OriginURL: noOrigin, In: strings.NewReader(""), Out: &bytes.Buffer{}})
		if _, err := os.Stat(filepath.Join(dir, ".agent-harness")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused init left .agent-harness/ behind: %v", err)
		}
	})
}

// The Go Dockerfile installs the toolchain go.mod asks for, so `make check`
// does not run under a different Go than the project declares.
func TestGoDockerfileFollowsGoMod(t *testing.T) {
	dir := checkout(t, map[string]string{"go.mod": "module x\n\ngo 1.24\n"})
	if err := Run(Options{Dir: dir, Yes: true, OriginURL: origin("git@github.com:acme/widgets.git"), In: strings.NewReader(""), Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if df := read(t, filepath.Join(dir, ".agent-harness", "Dockerfile")); !strings.Contains(df, "ARG GO_VERSION=1.24.0\n") {
		t.Errorf("Dockerfile does not pin go.mod's Go:\n%s", df)
	}
}
