package sandbox

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/git"
)

// errFake stands in for the non-nil error `exec` returns on a failed command.
var errFake = errors.New("exit status 1")

// valuesForFlag pulls the value following each occurrence of flag (docker's
// `-v A:B` / `-e NAME` are positional pairs).
func valuesForFlag(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func baseConfig() Config {
	return Config{
		Image:           "herd-agent-harness:latest",
		HerdPath:        "/Users/dan/herd",
		FindingsDir:     "/Users/dan/herd/agent-harness/logs/run-1/findings/BEH-362-tdd",
		PnpmStoreVolume: "herd-pnpm-store",
		Prompt:          "/tdd Work on BEH-362.",
	}
}

func TestRunsClaudeWithPromptAndPrintFlags(t *testing.T) {
	c := baseConfig()
	args := BuildDockerRunArgs(c)

	if args[0] != "run" {
		t.Errorf("args[0] = %q, want run", args[0])
	}
	imageIdx := slices.Index(args, c.Image)
	if imageIdx < 0 {
		t.Fatal("image not found in args")
	}
	cmd := args[imageIdx+1:]

	for _, want := range []string{"claude", "-p", c.Prompt, "--dangerously-skip-permissions"} {
		if !slices.Contains(cmd, want) {
			t.Errorf("claude invocation missing %q", want)
		}
	}
	if !strings.Contains(strings.Join(cmd, " "), "--output-format stream-json") {
		t.Error("missing --output-format stream-json")
	}
}

// Review emits no findings (retrospective owns them), so it passes no
// FindingsDir — and the container must then mount no /findings dropbox at all,
// keeping the "do not write findings" steering honest (there's nowhere to write).
func TestOmitsFindingsMountWhenNoFindingsDir(t *testing.T) {
	c := baseConfig()
	c.FindingsDir = ""
	args := BuildDockerRunArgs(c)

	if slices.Contains(args, FindingsMountPath) {
		t.Error("findings mount path must not appear when FindingsDir is empty")
	}
	for _, m := range valuesForFlag(args, "-v") {
		if strings.HasSuffix(m, ":"+FindingsMountPath) {
			t.Errorf("no /findings bind mount expected, got %q", m)
		}
	}
}

func TestMountsFindingsWhenFindingsDirGiven(t *testing.T) {
	c := baseConfig() // FindingsDir is set
	mounts := valuesForFlag(BuildDockerRunArgs(c), "-v")

	if !slices.Contains(mounts, c.FindingsDir+":"+FindingsMountPath) {
		t.Errorf("findings mount expected when FindingsDir is set, got mounts: %v", mounts)
	}
}

// The throwaway gate-re-run container (DESIGN.md: harness re-runs the config-
// declared named gates on the branch) carries NO secrets — not even the Claude
// credential — and runs the config-supplied gate command verbatim in the worktree,
// not the main checkout. BEH-634 makes the command a parameter (was a hardcoded
// `pnpm check && pnpm typecheck`), so a Go/.NET Consumer differs only in config.
func TestGateRunArgsCarryNoSecretsAndRunGateCommandInWorktree(t *testing.T) {
	args := BuildGateRunArgs(GateConfig{
		Image:           "herd-agent-harness:latest",
		HerdPath:        "/Users/dan/herd",
		WorktreePath:    "/Users/dan/herd/.claude/worktrees/beh-371",
		PnpmStoreVolume: "herd-pnpm-store",
		ContainerName:   "herd-harness-gate-1",
	}, "pnpm run check")
	joined := strings.Join(args, " ")

	// No credential of any kind crosses into the gate container.
	for _, secret := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "LINEAR"} {
		if regexp.MustCompile(`(?i)` + secret).MatchString(joined) {
			t.Errorf("gate container must carry no secrets, but argv mentions %q: %v", secret, args)
		}
	}
	// Runs exactly the config-supplied gate command — verbatim, and nothing else.
	// The command is now the sole source of truth (no hardcoded install prefix, no
	// hardcoded typecheck): node_modules is pre-populated by the separate install
	// container (BEH-490), and herd's `pnpm run check` resolves from the worktree
	// root via the committed root package.json passthrough.
	if !strings.Contains(joined, "pnpm run check") {
		t.Errorf("gate args must run the supplied command verbatim, got: %v", args)
	}
	// The command param is the ONLY gate content — the builder must not smuggle in
	// a second hardcoded gate the caller didn't ask for.
	if strings.Contains(joined, "typecheck") {
		t.Errorf("gate must run only the supplied command, not a hardcoded typecheck: %v", args)
	}
	// It must NOT run the full `pnpm run build` — its vite bundling + prerender
	// crawl is memory-heavy and gets OOM-killed (exit 137) in the sandbox even on a
	// correct diff (BEH-407/477/491/519/529), which once blocked a green, reviewed
	// branch from shipping. That guard belongs to the config; the builder just must
	// not inject build itself.
	if strings.Contains(joined, "run build") {
		t.Errorf("gate must not run the OOM-prone `pnpm run build`, got: %v", args)
	}
	// Working dir is the worktree (the branch under review), not the main checkout.
	workdirs := valuesForFlag(args, "-w")
	if len(workdirs) == 0 || !strings.HasPrefix(workdirs[len(workdirs)-1], "/Users/dan/herd/.claude/worktrees/beh-371") {
		t.Errorf("gate must run in the worktree, got -w %v", workdirs)
	}
}

func TestGateRunArgsMountCheckoutAndPnpmStore(t *testing.T) {
	c := GateConfig{
		Image:           "herd-agent-harness:latest",
		HerdPath:        "/Users/dan/herd",
		WorktreePath:    "/Users/dan/herd/.claude/worktrees/beh-371",
		PnpmStoreVolume: "herd-pnpm-store",
	}
	mounts := valuesForFlag(BuildGateRunArgs(c, "pnpm run check"), "-v")

	// The whole checkout is bind-mounted at its real path so the worktree's
	// absolute .git pointer resolves; the pnpm store keeps install near-instant.
	for _, want := range []string{
		c.HerdPath + ":" + c.HerdPath,
		c.PnpmStoreVolume + ":" + PnpmStoreMountPath,
	} {
		if !slices.Contains(mounts, want) {
			t.Errorf("gate mounts missing %q, got: %v", want, mounts)
		}
	}
}

func TestGateRunArgsNameContainerForKill(t *testing.T) {
	args := BuildGateRunArgs(GateConfig{ContainerName: "herd-harness-gate-1"}, "pnpm run check")
	if !slices.Contains(valuesForFlag(args, "--name"), "herd-harness-gate-1") {
		t.Error("gate container must be nameable so the harness can kill it on timeout")
	}
}

// The implementation tool strips web/node_modules from the worktree on handoff
// (BEH-412), so the cold review SESSION would otherwise discover it missing and
// pay a full `pnpm install` mid-gate (BEH-490). The harness pre-populates it with
// a throwaway install container before the session: a frozen install in the
// worktree against the warm pnpm store — install ONLY, no check/build (that's the
// later ground-truth gate's job) — carrying no secrets.
func TestInstallRunArgsRunsFrozenInstallOnlyInWorktree(t *testing.T) {
	args := BuildInstallRunArgs(GateConfig{
		Image:           "herd-agent-harness:latest",
		HerdPath:        "/Users/dan/herd",
		WorktreePath:    "/Users/dan/herd/.claude/worktrees/beh-490",
		PnpmStoreVolume: "herd-pnpm-store",
		ContainerName:   "herd-harness-install-1",
	})
	joined := strings.Join(args, " ")

	// A frozen install (reproducible, lockfile-pinned), mirroring new-worktree.sh.
	if !strings.Contains(joined, "pnpm install --frozen-lockfile") {
		t.Errorf("install args must run `pnpm install --frozen-lockfile`, got: %v", args)
	}
	// Install ONLY — check/build belong to the separate ground-truth gate, not the
	// pre-session prep. Running them here would duplicate the slow gate needlessly.
	if strings.Contains(joined, "pnpm run check") || strings.Contains(joined, "pnpm run build") {
		t.Errorf("install prep must not run check/build, got: %v", args)
	}
	// No credential of any kind crosses into the prep container — it runs no model.
	for _, secret := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "LINEAR"} {
		if regexp.MustCompile(`(?i)` + secret).MatchString(joined) {
			t.Errorf("install container must carry no secrets, but argv mentions %q: %v", secret, args)
		}
	}
	// Installs into the worktree (the branch under review), not the main checkout.
	workdirs := valuesForFlag(args, "-w")
	if len(workdirs) == 0 || workdirs[len(workdirs)-1] != "/Users/dan/herd/.claude/worktrees/beh-490" {
		t.Errorf("install must run in the worktree, got -w %v", workdirs)
	}
	// Mounts the checkout (so the worktree's .git pointer resolves) and the warm
	// pnpm store (so the install is a near-instant hardlink op, not a fetch).
	mounts := valuesForFlag(args, "-v")
	for _, want := range []string{
		"/Users/dan/herd:/Users/dan/herd",
		"herd-pnpm-store:" + PnpmStoreMountPath,
	} {
		if !slices.Contains(mounts, want) {
			t.Errorf("install mounts missing %q, got: %v", want, mounts)
		}
	}
	// Nameable so the harness can kill it on timeout.
	if !slices.Contains(valuesForFlag(args, "--name"), "herd-harness-install-1") {
		t.Error("install container must be nameable so the harness can kill it on timeout")
	}
}

func TestPinsModelWhenGiven(t *testing.T) {
	c := baseConfig()
	c.Model = "opus"
	models := valuesForFlag(BuildDockerRunArgs(c), "--model")

	if !slices.Contains(models, "opus") {
		t.Errorf("--model opus should be emitted when a model is set, got: %v", models)
	}
}

func TestOmitsModelWhenAbsent(t *testing.T) {
	if slices.Contains(BuildDockerRunArgs(baseConfig()), "--model") {
		t.Error("--model should be omitted when no model is given")
	}
}

// The checkout is bind-mounted at its real host path (ADR-0002), not a synthetic
// container path, so a worktree's absolute `.git` pointer resolves identically in
// the container and on the host.
func TestBindMountsCheckoutAtRealHostPath(t *testing.T) {
	c := baseConfig()
	args := BuildDockerRunArgs(c)
	mounts := valuesForFlag(args, "-v")

	for _, want := range []string{
		c.HerdPath + ":" + c.HerdPath,
		c.PnpmStoreVolume + ":/pnpm-store",
		c.FindingsDir + ":" + FindingsMountPath,
	} {
		if !slices.Contains(mounts, want) {
			t.Errorf("mounts missing %q", want)
		}
	}

	if workdirs := valuesForFlag(args, "-w"); !slices.Contains(workdirs, c.HerdPath) {
		t.Errorf("workdir should be the real host path %q, got -w %v", c.HerdPath, workdirs)
	}
}

func TestPassesSandboxSecretsByNameButNeverLinear(t *testing.T) {
	args := BuildDockerRunArgs(baseConfig())
	envNames := valuesForFlag(args, "-e")

	if !slices.Contains(envNames, "ANTHROPIC_API_KEY") {
		t.Error("missing ANTHROPIC_API_KEY")
	}
	if !slices.Contains(envNames, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("missing CLAUDE_CODE_OAUTH_TOKEN — oat tokens need Bearer auth, not x-api-key")
	}
	// The container no longer pushes (ADR-0002): only the Claude credential crosses
	// the boundary. GH_TOKEN stays host-only for the harness's own push/PR.
	if slices.Contains(envNames, "GH_TOKEN") {
		t.Error("GH_TOKEN must not cross the sandbox boundary — the container no longer pushes")
	}
	if regexp.MustCompile(`(?i)GH_TOKEN`).MatchString(strings.Join(args, " ")) {
		t.Error("GH_TOKEN may not appear anywhere in the argv")
	}
	if slices.Contains(envNames, "LINEAR_API_KEY") {
		t.Error("LINEAR_API_KEY must never cross the boundary")
	}
	if regexp.MustCompile(`(?i)LINEAR`).MatchString(strings.Join(args, " ")) {
		t.Error("no Linear secret may appear anywhere in the argv")
	}
}

// The entrypoint matches the runtime uid to the checkout's owner by stat-ing the
// mount, so it must know where the checkout landed. With the mount now at the
// real host path, that path is passed in by value as HERD_PATH (not a secret).
func TestPassesMountPathToEntrypoint(t *testing.T) {
	c := baseConfig()
	envs := valuesForFlag(BuildDockerRunArgs(c), "-e")

	if !slices.Contains(envs, "HERD_PATH="+c.HerdPath) {
		t.Errorf("entrypoint needs the mount path: expected -e HERD_PATH=%s, got %v", c.HerdPath, envs)
	}
}

// BEH-579: the bind-mounted checkout carries a placeholder `Test
// <test@example.com>` LOCAL git config that overrides the entrypoint's `git
// config --global` identity, so the agent's in-container handoff commit was
// authored AND committed as Test — polluting `git blame`/contributor stats on
// every harness-built PR. The launcher stamps the harness bot identity via
// GIT_AUTHOR_*/GIT_COMMITTER_* env, which take precedence over every git config
// level, so the in-container commit carries an intentional author/committer.
func TestStampsHarnessGitIdentityForAgentCommits(t *testing.T) {
	args := BuildDockerRunArgs(baseConfig())

	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		if got := envValue(args, name); got != git.HarnessAuthorName {
			t.Errorf("%s = %q, want the bot identity %q (else the leaked Test identity wins)", name, got, git.HarnessAuthorName)
		}
	}
	for _, email := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		if got := envValue(args, email); got != git.HarnessAuthorEmail {
			t.Errorf("%s = %q, want the bot email %q", email, got, git.HarnessAuthorEmail)
		}
	}
}

func TestNamesContainerWhenGiven(t *testing.T) {
	c := baseConfig()
	c.ContainerName = "herd-harness-run1-tdd"
	names := valuesForFlag(BuildDockerRunArgs(c), "--name")

	if !slices.Contains(names, "herd-harness-run1-tdd") {
		t.Error("container not named")
	}
}

func TestOmitsNameWhenAbsent(t *testing.T) {
	if slices.Contains(BuildDockerRunArgs(baseConfig()), "--name") {
		t.Error("--name should be omitted when no container name is given")
	}
}

func TestSecretsPassedByNameOnly(t *testing.T) {
	args := BuildDockerRunArgs(baseConfig())

	if !slices.Contains(args, "ANTHROPIC_API_KEY") {
		t.Error("ANTHROPIC_API_KEY should be passed by name")
	}
	// `-e NAME` (docker reads the value from the harness env), not `-e NAME=value`.
	if strings.Contains(strings.Join(args, " "), "ANTHROPIC_API_KEY=") {
		t.Error("secret value must not be embedded in the argv")
	}
}

// envValue pulls the value of a `-e NAME=value` pair from the argv, returning
// "" if the var is absent or passed by name only (`-e NAME`).
func envValue(args []string, name string) string {
	for _, e := range valuesForFlag(args, "-e") {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// The agent runs new-worktree.sh through Claude Code's Bash tool, whose
// per-command deadline defaults to 2 min (BASH_DEFAULT_TIMEOUT_MS, per the CLI
// docs) — too short for the first-run 1428-file checkout + frozen install +
// Playwright Chromium on slow I/O, which spuriously timed out and burned a turn
// on a re-run (BEH-486, follow-up to BEH-480, where this sandbox hit a ~3m20s
// wall). The launcher raises that default by passing BASH_DEFAULT_TIMEOUT_MS into
// the container, generous enough to finish setup in one shot. The 200_000ms bound
// below is a conservative floor: above both the 2-min documented default and the
// ~3m20s historically observed in-sandbox.
func TestRaisesBashCommandDeadlineAboveTheTimingOutDefault(t *testing.T) {
	v := envValue(BuildDockerRunArgs(baseConfig()), "BASH_DEFAULT_TIMEOUT_MS")
	if v == "" {
		t.Fatal("BASH_DEFAULT_TIMEOUT_MS must be passed into the agent container so new-worktree.sh gets a generous deadline")
	}
	ms, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("BASH_DEFAULT_TIMEOUT_MS must be an integer ms value, got %q", v)
	}
	// Must comfortably exceed the short default that was timing out — 200_000ms is
	// a conservative floor above both the 2-min documented default and the ~3m20s
	// this sandbox historically hit (BEH-480).
	if ms <= 200_000 {
		t.Errorf("BASH_DEFAULT_TIMEOUT_MS = %d ms, want > 200000 (above the short default that timed out)", ms)
	}
}

// The raised default is only honoured if the ceiling is at least as high —
// Claude Code clamps a default above BASH_MAX_TIMEOUT_MS back down to the max,
// which would silently defeat the fix. The launcher must lift the ceiling too.
func TestBashTimeoutCeilingIsAtLeastTheRaisedDefault(t *testing.T) {
	args := BuildDockerRunArgs(baseConfig())
	def, err1 := strconv.Atoi(envValue(args, "BASH_DEFAULT_TIMEOUT_MS"))
	max, err2 := strconv.Atoi(envValue(args, "BASH_MAX_TIMEOUT_MS"))
	if err1 != nil || err2 != nil {
		t.Fatalf("both BASH_*_TIMEOUT_MS must be integer ms values, got default=%q max=%q",
			envValue(args, "BASH_DEFAULT_TIMEOUT_MS"), envValue(args, "BASH_MAX_TIMEOUT_MS"))
	}
	if max < def {
		t.Errorf("BASH_MAX_TIMEOUT_MS (%d) must be >= BASH_DEFAULT_TIMEOUT_MS (%d), else the raised default is clamped away", max, def)
	}
}

func TestDockerErrorReason(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"blank only", "\n  \n\t\n", ""},
		{"single", "hello", "hello"},
		{"trailing newline", "a\nb\n", "b"},
		{"trailing blanks trimmed", "real reason\n\n  \n", "real reason"},
		{"inner blanks ignored", "first\n\nlast", "last"},
		{"trims the returned line", "  spaced  \n", "spaced"},
		// docker prints the real cause, then a generic help trailer — skip it.
		{
			"skips help trailer",
			"Unable to find image 'x:latest' locally\ndocker: Error response from daemon: pull access denied for x.\nSee 'docker run --help'.",
			"docker: Error response from daemon: pull access denied for x.",
		},
		{"help trailer only falls through to prior line", "the cause\nSee 'docker run --help'", "the cause"},
		{"all help trailers yield empty", "See 'docker run --help'.\nSee 'docker run --help'.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DockerErrorReason(c.in); got != c.want {
				t.Errorf("DockerErrorReason(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsRetryableStartFailure(t *testing.T) {
	cases := []struct {
		name, reason string
		want         bool
	}{
		// The reported BEH-542 signature: overlay2 teardown failing because the
		// store went read-only under disk/IO pressure — environmental & transient.
		{
			"overlay2 read-only teardown",
			`driver "overlay2" failed to remove root filesystem: unlinkat /var/lib/docker/overlay2/abc/merged: read-only file system`,
			true,
		},
		{"bare read-only file system", "write /var/lib/docker/...: read-only file system", true},
		{"mixed case EROFS", "Read-Only File System", true},
		// The reported BEH-547/BEH-550 signature: the container vanished mid-session
		// under memory pressure and the engine reported the wait failing on a severed
		// stream — environmental & transient, the same class as the 137 OOM-kill but
		// surfaced as an exit-125 launch failure, recoverable on a bare retry.
		{"container wait unexpected EOF", "error waiting for container: unexpected EOF", true},
		{"wrapped container wait EOF", `level=error msg="error waiting for container: unexpected EOF"`, true},
		{"mixed case container wait EOF", "error Waiting For Container: Unexpected EOF", true},
		// Genuine, terminal 125s carry none of the transient signatures.
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", false},
		{"image missing", "Unable to find image 'herd-agent-harness:latest' locally", false},
		{"bad flag", "unknown flag: --nope", false},
		// A bare "unexpected EOF" without the container-wait context is a config/parse
		// fault, not the engine wedge — terminal, so the match stays anchored on
		// "waiting for container".
		{"unrelated parse EOF", "yaml: line 3: unexpected EOF", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryableStartFailure(c.reason); got != c.want {
				t.Errorf("IsRetryableStartFailure(%q) = %v, want %v", c.reason, got, c.want)
			}
		})
	}
}

const (
	testImage   = "herd-agent-harness:latest"
	testContext = "/herd/agent-harness"
)

// reply is one canned docker response. A reply of {nil,nil} means "succeeds
// with no output".
type reply struct {
	out []byte
	err error
}

// fakeDocker routes by the docker sub-command so a test can fail just `info` or
// just `image inspect` while letting the other pass. The "image" verb accepts a
// *sequence* of replies (popped per call) so a test can model "inspect fails,
// then — after a build — succeeds"; a single reply is reused for every call.
func fakeDocker(byVerb map[string][]reply, calls *[]string) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		if calls != nil {
			*calls = append(*calls, strings.Join(append([]string{name}, args...), " "))
		}
		verb := ""
		if len(args) > 0 {
			verb = args[0] // "info" or "image"
		}
		seq := byVerb[verb]
		if len(seq) == 0 {
			return nil, nil
		}
		r := seq[0]
		if len(seq) > 1 {
			byVerb[verb] = seq[1:] // consume; last reply sticks
		}
		return r.out, r.err
	}
}

// one wraps a single reply reused for every call to a verb.
func one(out []byte, err error) []reply { return []reply{{out, err}} }

// noBuild is a builder that fails the test if invoked — for the paths where the
// image is already present and no build should happen.
func noBuild(t *testing.T) func(string, string) error {
	return func(image, _ string) error {
		t.Helper()
		t.Errorf("build should not be called when the image is present (image=%q)", image)
		return nil
	}
}

func TestPreflightOKWhenDaemonAndImagePresent(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte(`[{"Id":"sha256:abc"}]`), nil),
	}, nil)
	if err := Preflight(testImage, testContext, run, noBuild(t), plentyDisk); err != nil {
		t.Errorf("Preflight should pass when daemon is up and image is present, got: %v", err)
	}
}

func TestPreflightSurfacesDaemonDownReason(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info": one(
			[]byte("Client: Docker Engine\n\nCannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"),
			errFake,
		),
	}, nil)
	err := Preflight(testImage, testContext, run, noBuild(t), plentyDisk)
	if err == nil {
		t.Fatal("Preflight should fail when the daemon is unreachable")
	}
	if !strings.Contains(err.Error(), "Is the docker daemon running?") {
		t.Errorf("error should carry docker's own reason, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Docker Desktop") {
		t.Errorf("error should hint at Docker Desktop, got: %v", err)
	}
}

func TestPreflightBuildsImageOnMiss(t *testing.T) {
	var builtImage, builtContext string
	builds := 0
	build := func(image, buildContext string) error {
		builds++
		builtImage, builtContext = image, buildContext
		return nil
	}
	// inspect fails first (missing), then succeeds (after the build).
	run := fakeDocker(map[string][]reply{
		"info": one([]byte("Server Version: 27.0.0"), nil),
		"image": {
			{[]byte("Error: No such image: herd-agent-harness:latest"), errFake},
			{[]byte(`[{"Id":"sha256:abc"}]`), nil},
		},
	}, nil)
	if err := Preflight(testImage, testContext, run, build, plentyDisk); err != nil {
		t.Fatalf("Preflight should build the image on miss and pass, got: %v", err)
	}
	if builds != 1 {
		t.Errorf("build should be called exactly once, got %d", builds)
	}
	if builtImage != testImage || builtContext != testContext {
		t.Errorf("build called with (%q, %q), want (%q, %q)", builtImage, builtContext, testImage, testContext)
	}
}

func TestPreflightSurfacesBuildFailure(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte("Error: No such image: herd-agent-harness:latest"), errFake),
	}, nil)
	build := func(string, string) error { return errFake }
	err := Preflight(testImage, testContext, run, build, plentyDisk)
	if err == nil {
		t.Fatal("Preflight should fail when the build fails")
	}
	if !strings.Contains(err.Error(), testImage) || !strings.Contains(err.Error(), testContext) {
		t.Errorf("error should name the image and build context, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker build") {
		t.Errorf("error should give the manual build command, got: %v", err)
	}
}

func TestPreflightFailsWhenImageAbsentAfterBuild(t *testing.T) {
	// A build that "succeeds" but produces no such tag (wrong context/Dockerfile)
	// must still fail rather than fall through to a doomed `docker run`.
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte("Error: No such image: herd-agent-harness:latest"), errFake),
	}, nil)
	build := func(string, string) error { return nil }
	err := Preflight(testImage, testContext, run, build, plentyDisk)
	if err == nil {
		t.Fatal("Preflight should fail when the image is still absent after a 'successful' build")
	}
	if !strings.Contains(err.Error(), "still not present after build") {
		t.Errorf("error should flag the post-build absence, got: %v", err)
	}
}

func TestPreflightDoesNotInspectImageWhenDaemonDown(t *testing.T) {
	var calls []string
	run := fakeDocker(map[string][]reply{
		"info": one(nil, errFake),
	}, &calls)
	_ = Preflight(testImage, testContext, run, noBuild(t), plentyDisk)
	// A down daemon should short-circuit before the image check — no point
	// inspecting an image we can't run anyway.
	if len(calls) != 1 || calls[0] != "docker info" {
		t.Errorf("Preflight should stop after `docker info` fails, got calls: %v", calls)
	}
}

func TestPreflightProbesDaemonNotClientVersion(t *testing.T) {
	var calls []string
	run := fakeDocker(map[string][]reply{
		"info":  one(nil, nil),
		"image": one(nil, nil),
	}, &calls)
	_ = Preflight(testImage, testContext, run, noBuild(t), plentyDisk)
	// `docker info` round-trips to the daemon; `docker --version` is client-only
	// and would pass even with the daemon down.
	if len(calls) == 0 || calls[0] != "docker info" {
		t.Errorf("Preflight should probe with `docker info` first, got: %v", calls)
	}
	if len(calls) < 2 || calls[1] != "docker image inspect "+testImage {
		t.Errorf("Preflight should then inspect the image, got: %v", calls)
	}
}

func TestPreflightFallsBackToErrWhenNoOutput(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info": one(nil, errFake),
	}, nil)
	err := Preflight(testImage, testContext, run, noBuild(t), plentyDisk)
	if err == nil || !strings.Contains(err.Error(), errFake.Error()) {
		t.Errorf("Preflight should fall back to the runner error when there is no output, got: %v", err)
	}
}

// plentyDisk / scantDisk are diskFree stand-ins for Preflight: one reports far
// more free space than the floor, the other far less. The disk-agnostic Preflight
// tests pass plentyDisk so the new precondition never trips them.
func plentyDisk(string) (uint64, error) { return MinFreeDiskBytes * 4, nil }
func scantDisk(string) (uint64, error)  { return MinFreeDiskBytes / 2, nil }

func TestPreflightRefusesToLaunchWhenDiskBelowFloor(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte(`[{"Id":"sha256:abc"}]`), nil),
	}, nil)
	err := Preflight(testImage, testContext, run, noBuild(t), scantDisk)
	if err == nil {
		t.Fatal("Preflight should refuse to launch when free disk is below the floor")
	}
}

// The disk check is the cheapest precondition and the root cause of the opaque
// downstream docker failures — so it must short-circuit before any docker call,
// not after a (possibly slow / wedged) `docker info`.
func TestPreflightChecksDiskBeforeTouchingDocker(t *testing.T) {
	var calls []string
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte(`[{"Id":"sha256:abc"}]`), nil),
	}, &calls)
	_ = Preflight(testImage, testContext, run, noBuild(t), scantDisk)
	if len(calls) != 0 {
		t.Errorf("Preflight should fail on the disk floor before any docker call, got calls: %v", calls)
	}
}

// The actionable message must name how much is free, where, and the reclaim steps
// — otherwise the operator is left with a bare "insufficient disk" and no next move.
func TestPreflightDiskErrorIsActionable(t *testing.T) {
	run := fakeDocker(map[string][]reply{}, nil)
	err := Preflight(testImage, testContext, run, noBuild(t), scantDisk)
	if err == nil {
		t.Fatal("expected a disk-floor error")
	}
	// The two pnpm/worktree reclaims are often NOT the real culprit for the
	// harness — its `docker run` sandboxes accrete stale build cache /
	// unreferenced images, which is frequently the largest consumer — so the hint
	// must also point at the Docker reclaims (BEH-566).
	for _, want := range []string{testContext, "MiB", "pnpm store prune", "prune-merged-worktrees", "docker builder prune", "docker system prune"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("disk error should mention %q, got: %v", want, err)
		}
	}
}

// A statfs failure (unreadable path, exotic FS) must NOT block a launch — free
// space simply couldn't be read, which is not evidence the disk is full. Preflight
// falls through to the docker probes.
func TestPreflightProceedsWhenDiskProbeErrors(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte(`[{"Id":"sha256:abc"}]`), nil),
	}, nil)
	probeErr := func(string) (uint64, error) { return 0, errFake }
	if err := Preflight(testImage, testContext, run, noBuild(t), probeErr); err != nil {
		t.Errorf("a statfs error should not block launch, got: %v", err)
	}
}

// Exactly at the floor is enough — the guard only refuses strictly below it.
func TestPreflightAllowsLaunchAtTheFloor(t *testing.T) {
	run := fakeDocker(map[string][]reply{
		"info":  one([]byte("Server Version: 27.0.0"), nil),
		"image": one([]byte(`[{"Id":"sha256:abc"}]`), nil),
	}, nil)
	atFloor := func(string) (uint64, error) { return MinFreeDiskBytes, nil }
	if err := Preflight(testImage, testContext, run, noBuild(t), atFloor); err != nil {
		t.Errorf("Preflight should allow launch with free space exactly at the floor, got: %v", err)
	}
}
