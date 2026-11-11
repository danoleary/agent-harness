package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// formatVerbRe matches a fmt verb with its flags, width and precision: where a
// warning's fixed text stops and a run's own values begin.
var formatVerbRe = regexp.MustCompile(`%[-+# 0]*[0-9*]*(\.[0-9*]+)?[a-zA-Z%]`)

// warnFragment returns the stable text of a "⚠ " run-log warning literal, the
// part an operator searches errors.md for: the first stretch between format
// verbs that runs to minFragment characters or more, trimmed of the spaces and
// brackets that only join it to a value. A literal that is bare "⚠ " (the rest
// concatenated on) or holds no stretch that long has none, and returns "".
func warnFragment(lit string) string {
	const minFragment = 12
	for _, seg := range formatVerbRe.Split(strings.TrimPrefix(lit, "⚠ "), -1) {
		if seg = strings.Trim(seg, " (\"'`:"); len([]rune(seg)) >= minFragment {
			return seg
		}
	}
	return ""
}

func TestWarnFragment(t *testing.T) {
	for _, c := range []struct{ lit, want string }{
		{"⚠ could not write the red-base finding (", "could not write the red-base finding"},
		{"⚠ post_create exited %d — the worktree may lack toolchain deps", "post_create exited"},
		{"⚠ base gate %q failed environmentally (exit %d: an OOM-kill", "failed environmentally (exit"},
		{"⚠ %s cites Sentry %s but it could not be fetched: %s", "cites Sentry"},
		{"⚠ ", ""},
		{"⚠ %s: %s", ""},
	} {
		if got := warnFragment(c.lit); got != c.want {
			t.Errorf("warnFragment(%q) = %q, want %q", c.lit, got, c.want)
		}
	}
}

// TestEveryWarningHasAnErrorsRow keeps docs/reference/errors.md the catalogue
// of the ⚠ warnings the run log can carry (#300): every string literal under
// internal/ that opens with "⚠ " must have its stable fragment somewhere in that
// page, so a new warning lands with a row saying what causes it and what to do.
// internal/viewer is left out: its ⚠ lines are dashboard banners over the run's
// state, not run-log messages. A warning whose literal is bare "⚠ " (a session
// Warning, a releaseClaim reason) carries no text of its own to check.
func TestEveryWarningHasAnErrorsRow(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "reference", "errors.md"))
	if err != nil {
		t.Fatal(err)
	}
	// errors.md writes a run's own values as "…"; normalise the curly quotes the
	// prose might use so a fragment holding a quote still matches.
	catalogue := strings.NewReplacer("“", `"`, "”", `"`).Replace(string(b))

	found := 0
	fset := token.NewFileSet()
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "viewer" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(s, "⚠ ") {
				return true
			}
			found++
			if frag := warnFragment(s); frag != "" && !strings.Contains(catalogue, frag) {
				rel, _ := filepath.Rel(root, fset.Position(lit.Pos()).Filename)
				t.Errorf("%s:%d: the warning fragment %q has no row in docs/reference/errors.md; add one with its cause and fix",
					filepath.ToSlash(rel), fset.Position(lit.Pos()).Line, frag)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal(`found no "⚠ " string literal under internal/; has the warning prefix changed?`)
	}
}
