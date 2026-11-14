package config

import "testing"

// A gate's skip_when_only list is read from its [[gates]] entry (#221).
func TestLoadProjectReadsAGatesSkipWhenOnly(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "github"
repo = "o/r"
[[gates]]
name = "check"
command = "make check"
skip_when_only = [".github/**", "*.md"]
[[gates]]
name = "build"
command = "make build"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got := pc.Gates[0].SkipWhenOnly; len(got) != 2 || got[0] != ".github/**" || got[1] != "*.md" {
		t.Errorf("gates[0].skip_when_only = %q", got)
	}
	if got := pc.Gates[1].SkipWhenOnly; got != nil {
		t.Errorf("gates[1].skip_when_only = %q, want none", got)
	}
}

// A pattern path.Match cannot parse, an empty one or an absolute one is a config
// error at load, not a silent never-match at the first PR fix.
func TestLoadProjectRejectsABadSkipWhenOnlyPattern(t *testing.T) {
	for _, pattern := range []string{"[.github", "", "/.github/**"} {
		dir := t.TempDir()
		writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "github"
repo = "o/r"
[[gates]]
name = "check"
command = "make check"
skip_when_only = ["`+pattern+`"]
`)
		_, err := LoadProject(dir)
		assertMentions(t, err, `gates[0] ("check") skip_when_only`)
	}
}

func TestGateSkipsOnlyWhenEveryPathMatches(t *testing.T) {
	g := Gate{Name: "check", SkipWhenOnly: []string{".github/**", "docs/*.md"}}
	cases := []struct {
		paths []string
		want  bool
	}{
		{[]string{".github/workflows/ci.yml"}, true},
		{[]string{".github/dependabot.yml", ".github/workflows/ci.yml"}, true},
		{[]string{"docs/a.md"}, true},
		{[]string{"docs/sub/a.md"}, false},
		{[]string{".github/workflows/ci.yml", "internal/fix/fix.go"}, false},
		{[]string{".githubx/a"}, false},
		{[]string{"a/.github/x"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := g.SkipsOver(c.paths); got != c.want {
			t.Errorf("SkipsOver(%q) = %v, want %v", c.paths, got, c.want)
		}
	}
	if (Gate{Name: "build"}).SkipsOver([]string{".github/workflows/ci.yml"}) {
		t.Error("a gate with no skip_when_only must never skip")
	}
}

// `**` matches any number of whole segments, zero included, anywhere in a pattern.
func TestGlobDoubleStarMatchesWholeSegments(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/how-to/x.md", true},
		{"docs/**/x.md", "docs/x.md", true},
		{"docs/**/x.md", "docs/a/b/x.md", true},
		{"docs/**/x.md", "docs/a/b/y.md", false},
		{".github/**", ".github", true},
		{"*.yml", "a/b.yml", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// A gate's cache_isolate list is read from its [[gates]] entry (#314), and a
// gate without the key isolates nothing.
func TestLoadProjectReadsAGatesCacheIsolate(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "github"
repo = "o/r"
[[gates]]
name = "test-storybook"
command = "pnpm test-storybook"
cache_isolate = ["web/node_modules/.vite", "web/node_modules/.cache/storybook"]
[[gates]]
name = "build"
command = "make build"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := pc.Gates[0].CacheIsolate; len(got) != 2 || got[0] != "web/node_modules/.vite" || got[1] != "web/node_modules/.cache/storybook" {
		t.Errorf("gates[0].cache_isolate = %q", got)
	}
	if got := pc.Gates[1].CacheIsolate; got != nil {
		t.Errorf("gates[1].cache_isolate = %q, want none", got)
	}
}

// A cache_isolate path must name a directory strictly inside the worktree: an
// empty, absolute, escaping or whole-worktree path is a config error at load,
// not a mount over the wrong tree at the first gate run.
func TestLoadProjectRejectsACacheIsolatePathOutsideTheWorktree(t *testing.T) {
	for _, p := range []string{"", "/tmp/vite", "../vite", "web/../../vite", ".", "web/..", "web,dst=/x"} {
		dir := t.TempDir()
		writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "github"
repo = "o/r"
[[gates]]
name = "check"
command = "make check"
cache_isolate = ["`+p+`"]
`)
		_, err := LoadProject(dir)
		assertMentions(t, err, `gates[0] ("check") cache_isolate path "`+p+`"`)
	}
}
