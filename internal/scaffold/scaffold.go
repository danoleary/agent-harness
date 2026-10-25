// Package scaffold writes a new Consumer's `.agent-harness/` directory — the
// work behind `agent-harness init` (ADR-0021).
//
// It turns two facts about a checkout — its toolchain and its GitHub remote —
// into a config.toml, a Dockerfile FROM the pinned base, the three Project bodies
// with placeholder skills, and a .env from the credential template. Everything it
// writes loads through the real config loader; nothing it writes is a decision
// the Consumer has not been asked to check (each file carries PlaceholderMarker
// where one is owed).
package scaffold

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/danoleary/agent-harness/internal/config"
)

// PlaceholderMarker tags every spot in a scaffolded file the Consumer still owes
// an edit — the skill names above all, which the harness cannot know.
const PlaceholderMarker = "TODO(agent-harness init)"

// NextCommand is what a scaffolded Consumer runs next: it checks the config,
// the prompt bodies, the credentials and the tracker, and prints a fix for each
// problem — the placeholder skill names included.
const NextCommand = "agent-harness doctor"

//go:embed templates
var templates embed.FS

var (
	envTemplate        = mustRead("templates/env.example")
	dockerfileTemplate = mustRead("templates/Dockerfile.tmpl")
)

func mustRead(name string) string {
	raw, err := templates.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// Options is one `agent-harness init` invocation.
type Options struct {
	// Dir is the checkout root the .agent-harness/ dir is written into.
	Dir string
	// Yes accepts every detected default without asking.
	Yes bool
	// Force overwrites files that already exist. The .env is never overwritten:
	// it is the one file that holds credentials.
	Force bool
	// Toolchain and Repo, when set, answer their question up front.
	Toolchain string
	Repo      string
	// OriginURL reads the checkout's origin remote URL.
	OriginURL func(dir string) (string, error)
	In        io.Reader
	Out       io.Writer
}

// Run scaffolds o.Dir's .agent-harness/. Every answer is settled before the
// first file is written, so a refusal leaves the checkout untouched.
func Run(o Options) error {
	ask := newAsker(o)

	detected, _ := Detect(o.Dir)
	name, err := ask.question(
		fmt.Sprintf("Toolchain (%s)", strings.Join(toolchainNames(), ", ")),
		o.Toolchain, detected.Name,
		func(s string) error {
			if _, ok := byName(o.Dir, s); !ok {
				return fmt.Errorf("unknown toolchain %q: pick one of %s", s, strings.Join(toolchainNames(), ", "))
			}
			return nil
		},
		"no toolchain detected (no go.mod, pnpm-lock.yaml, *.sln/*.csproj or pyproject.toml/requirements.txt at the root): pass --toolchain "+strings.Join(toolchainNames(), "|"))
	if err != nil {
		return err
	}
	tc, _ := byName(o.Dir, name)

	var remote string
	if o.OriginURL != nil {
		url, _ := o.OriginURL(o.Dir)
		remote = GitHubRepo(url)
	}
	repo, err := ask.question("GitHub repo whose issues are the tickets (owner/name)", o.Repo, remote,
		func(s string) error {
			_, _, err := config.SplitOwnerRepo(s)
			return err
		},
		"no GitHub origin remote to take tracker.repo from: pass --repo owner/name")
	if err != nil {
		return err
	}

	if !o.Yes {
		fmt.Fprintln(o.Out)
	}

	files, err := render(tc, repo, o.Dir)
	if err != nil {
		return err
	}

	w := writer{out: o.Out, force: o.Force}
	for _, f := range files {
		if err := w.write(filepath.Join(o.Dir, f.path), f.body, f.mode, f.keepAlways); err != nil {
			return err
		}
	}
	if err := ignoreRuntimeFiles(filepath.Join(o.Dir, ".gitignore"), o.Out); err != nil {
		return err
	}

	if len(w.kept) > 0 {
		fmt.Fprintf(o.Out, "\nKept %d existing file(s); rerun with --force to overwrite them (the .env is never overwritten).\n", len(w.kept))
	}
	fmt.Fprintf(o.Out, "\nNext:\n"+
		"  1. Put your credentials in %s/.env.\n"+
		"  2. Replace the /TODO-…-skill names in %s/prompts/ with skills this repo carries,\n"+
		"     and check every %s in config.toml and the Dockerfile.\n"+
		"  3. Check the setup, claiming nothing:\n\n"+
		"     %s\n",
		config.ProjectDirName, config.ProjectDirName, PlaceholderMarker, NextCommand)
	return nil
}

// file is one scaffolded file, relative to the checkout.
type file struct {
	path string
	body string
	mode fs.FileMode
	// keepAlways refuses the overwrite even under --force.
	keepAlways bool
}

// render produces every file init writes, from the toolchain and the repo.
func render(tc Toolchain, repo, dir string) ([]file, error) {
	_, project, _ := config.SplitOwnerRepo(repo)
	project = dockerName(project)

	type cache struct{ Volume, Path, Prune string }
	data := struct {
		Toolchain
		Repo, Image string
		Cache       *cache
	}{Toolchain: tc, Repo: repo, Image: project + "-agent-harness:latest"}
	if tc.Cache.Path != "" {
		data.Cache = &cache{Volume: project + "-" + tc.Cache.Suffix, Path: tc.Cache.Path, Prune: tc.Cache.Prune}
	}

	pd := config.ProjectDirName
	out := []file{
		{path: filepath.Join(pd, "config.toml"), body: "templates/config.toml.tmpl"},
		{path: filepath.Join(pd, "Dockerfile"), body: "templates/Dockerfile.tmpl"},
		{path: filepath.Join(pd, "prompts", "implement.md"), body: "templates/prompts/implement.md"},
		{path: filepath.Join(pd, "prompts", "review.md"), body: "templates/prompts/review.md"},
		{path: filepath.Join(pd, "prompts", "retro.md"), body: "templates/prompts/retro.md"},
	}
	for i := range out {
		body, err := execute(out[i].body, data)
		if err != nil {
			return nil, err
		}
		out[i].body, out[i].mode = body, 0o644
	}

	// The checkout this .env sits in is the project it drives.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	env := regexp.MustCompile(`(?m)^PROJECT_PATH=.*$`).ReplaceAllLiteralString(envTemplate, "PROJECT_PATH="+abs)
	return append(out, file{path: filepath.Join(pd, ".env"), body: env, mode: 0o600, keepAlways: true}), nil
}

// execute renders an embedded template. The delimiters are << >> because the
// Project bodies are themselves Go templates the harness renders per ticket
// ({{.Identifier}}), and config.toml is full of [[ ]].
func execute(name string, data any) (string, error) {
	t, err := template.New(filepath.Base(name)).Delims("<<", ">>").Funcs(template.FuncMap{
		"toml": tomlString,
		"tomlList": func(xs []string) string {
			quoted := make([]string, len(xs))
			for i, x := range xs {
				quoted[i] = tomlString(x)
			}
			return "[" + strings.Join(quoted, ", ") + "]"
		},
	}).ParseFS(templates, name)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}

// dockerName lowercases a repo name into something a Docker image or volume
// name accepts.
func dockerName(s string) string {
	s = regexp.MustCompile(`[^a-z0-9_.-]+`).ReplaceAllString(strings.ToLower(s), "-")
	if s = strings.Trim(s, "-._"); s == "" {
		return "project"
	}
	return s
}

// writer writes scaffolded files, keeping any that exist unless forced.
type writer struct {
	out   io.Writer
	force bool
	kept  []string
}

func (w *writer) write(path, body string, mode fs.FileMode, keepAlways bool) error {
	if _, err := os.Stat(path); err == nil && (!w.force || keepAlways) {
		w.kept = append(w.kept, path)
		fmt.Fprintf(w.out, "  kept   %s (exists)\n", path)
		return nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return err
	}
	// WriteFile leaves an existing file's mode alone; a forced rewrite still
	// lands on the mode the file is meant to have.
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	fmt.Fprintf(w.out, "  wrote  %s\n", path)
	return nil
}

// runtimeIgnores are the .gitignore lines for what the harness writes into
// .agent-harness/ at run time. The .env is first: the sandbox mask keeps the
// session from reading it, but only the ignore keeps it out of git history.
var runtimeIgnores = []string{
	config.ProjectDirName + "/.env",
	config.ProjectDirName + "/logs/",
	config.ProjectDirName + "/harness-findings/",
	config.ProjectDirName + "/STOP",
	config.ProjectDirName + "/loop.log",
	config.ProjectDirName + "/loop.pid",
}

// ignoreRuntimeFiles appends whichever runtimeIgnores lines the .gitignore lacks,
// creating it if need be. It only ever appends.
func ignoreRuntimeFiles(path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, line := range runtimeIgnores {
		if !have[line] && !have["/"+line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	if len(raw) > 0 && !bytes.HasSuffix(raw, []byte("\n")) {
		b.WriteString("\n")
	}
	if len(raw) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("# agent-harness: credentials and run-time artifacts.\n")
	for _, line := range missing {
		b.WriteString(line + "\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(out, "  added  %s to %s\n", strings.Join(missing, ", "), path)
	return nil
}

// asker settles one answer per question: a flag value if given, else the
// default under --yes, else whatever the operator types (an empty line takes
// the default).
type asker struct {
	yes bool
	in  *bufio.Reader
	out io.Writer
}

func newAsker(o Options) *asker {
	in := o.In
	if in == nil {
		in = strings.NewReader("")
	}
	return &asker{yes: o.Yes, in: bufio.NewReader(in), out: o.Out}
}

// question returns the settled answer. noDefault is the error for --yes (or a
// closed input) with nothing to default to.
func (a *asker) question(prompt, flag, def string, valid func(string) error, noDefault string) (string, error) {
	if flag != "" {
		return flag, valid(flag)
	}
	if a.yes {
		if def == "" {
			return "", errors.New(noDefault)
		}
		return def, nil
	}
	for {
		if def != "" {
			fmt.Fprintf(a.out, "%s [%s]: ", prompt, def)
		} else {
			fmt.Fprintf(a.out, "%s: ", prompt)
		}
		line, err := a.in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			answer = def
		}
		if answer != "" {
			verr := valid(answer)
			if verr == nil {
				return answer, nil
			}
			fmt.Fprintf(a.out, "  %v\n", verr)
		}
		if err != nil {
			if answer == "" {
				return "", errors.New(noDefault)
			}
			return "", fmt.Errorf("%s: no valid answer before the input closed", prompt)
		}
	}
}

var githubRemote = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// GitHubRepo returns the "owner/name" of a GitHub remote URL, or "" for any
// other host.
func GitHubRepo(url string) string {
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}
