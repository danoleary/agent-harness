package sandbox

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
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

const testImage = "herd-agent-harness:latest"

// fakeDocker routes by the docker sub-command so a test can fail just `info` or
// just `image inspect` while letting the other pass. A reply of {nil,nil} means
// "succeeds with no output".
type reply struct {
	out []byte
	err error
}

func fakeDocker(byVerb map[string]reply, calls *[]string) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		if calls != nil {
			*calls = append(*calls, strings.Join(append([]string{name}, args...), " "))
		}
		verb := ""
		if len(args) > 0 {
			verb = args[0] // "info" or "image"
		}
		r := byVerb[verb]
		return r.out, r.err
	}
}

func TestPreflightOKWhenDaemonAndImagePresent(t *testing.T) {
	run := fakeDocker(map[string]reply{
		"info":  {[]byte("Server Version: 27.0.0"), nil},
		"image": {[]byte(`[{"Id":"sha256:abc"}]`), nil},
	}, nil)
	if err := Preflight(testImage, run); err != nil {
		t.Errorf("Preflight should pass when daemon is up and image is present, got: %v", err)
	}
}

func TestPreflightSurfacesDaemonDownReason(t *testing.T) {
	run := fakeDocker(map[string]reply{
		"info": {
			[]byte("Client: Docker Engine\n\nCannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"),
			errFake,
		},
	}, nil)
	err := Preflight(testImage, run)
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

func TestPreflightFailsWhenImageMissing(t *testing.T) {
	run := fakeDocker(map[string]reply{
		"info":  {[]byte("Server Version: 27.0.0"), nil},
		"image": {[]byte("Error: No such image: herd-agent-harness:latest"), errFake},
	}, nil)
	err := Preflight(testImage, run)
	if err == nil {
		t.Fatal("Preflight should fail when the sandbox image is absent")
	}
	if !strings.Contains(err.Error(), testImage) {
		t.Errorf("error should name the missing image, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker build") {
		t.Errorf("error should tell the operator how to build it, got: %v", err)
	}
}

func TestPreflightDoesNotInspectImageWhenDaemonDown(t *testing.T) {
	var calls []string
	run := fakeDocker(map[string]reply{
		"info": {nil, errFake},
	}, &calls)
	_ = Preflight(testImage, run)
	// A down daemon should short-circuit before the image check — no point
	// inspecting an image we can't run anyway.
	if len(calls) != 1 || calls[0] != "docker info" {
		t.Errorf("Preflight should stop after `docker info` fails, got calls: %v", calls)
	}
}

func TestPreflightProbesDaemonNotClientVersion(t *testing.T) {
	var calls []string
	run := fakeDocker(map[string]reply{
		"info":  {nil, nil},
		"image": {nil, nil},
	}, &calls)
	_ = Preflight(testImage, run)
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
	run := fakeDocker(map[string]reply{
		"info": {nil, errFake},
	}, nil)
	err := Preflight(testImage, run)
	if err == nil || !strings.Contains(err.Error(), errFake.Error()) {
		t.Errorf("Preflight should fall back to the runner error when there is no output, got: %v", err)
	}
}
