package selfbuild

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const harness = "github.com/danoleary/agent-harness"

// checkoutOf is a Consumer checkout whose go.mod declares module.
func checkoutOf(t *testing.T, module string) string {
	t.Helper()
	dir := t.TempDir()
	if module != "" {
		body := "module " + module + "\n\ngo 1.26\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
			t.Fatalf("go.mod: %v", err)
		}
	}
	return dir
}

// mainLacking is origin/main as the count sees it: n harness commits the build
// lacks, or err.
func mainLacking(n int, err error, asked *[]string) CommitsSince {
	return func(commit string, paths ...string) (int, error) {
		if asked != nil {
			*asked = append(*asked, commit)
			*asked = append(*asked, paths...)
		}
		return n, err
	}
}

// #461: the third run of a ticket already resolved on main was cut from the very
// commit that fixed the re-dispatch, and still behaved as before it, because the
// host ran a binary built earlier (a9e5c7378763). Nothing said so. A build behind
// origin/main's own harness commits now names its commit and how many it lacks,
// and says to rebuild.
func TestStaleWarnsWhenTheHarnessBinaryLacksMainsHarnessCommits(t *testing.T) {
	var asked []string
	b := Build{Module: harness, Revision: "a9e5c73787634f0e1d2c3b4a5968778695a4b3c2"}
	got := Stale(b, checkoutOf(t, harness), mainLacking(1, nil, &asked))

	for _, want := range []string{"⚠", "a9e5c7378763", "lacks 1 harness commit on origin/main", "make build"} {
		if !strings.Contains(got, want) {
			t.Errorf("Stale = %q; want it to name %q", got, want)
		}
	}
	if !slices.Equal(asked, []string{b.Revision, "internal", "cmd"}) {
		t.Errorf("counted %v; want main's commits since the build's full revision under internal/ and cmd/", asked)
	}
	if got := Stale(b, checkoutOf(t, harness), mainLacking(3, nil, nil)); !strings.Contains(got, "lacks 3 harness commits") {
		t.Errorf("Stale = %q; want the plural count", got)
	}
}

// A build equal to or ahead of origin/main lacks nothing; a non-harness Consumer,
// or a binary that cannot say what it was built from, never warns; and a count
// that cannot run (a build commit this checkout never fetched) stays quiet rather
// than guess.
func TestStaleIsQuietWhenTheBuildIsCurrentOrNotTheConsumers(t *testing.T) {
	current := Build{Module: harness, Revision: "e7127d2aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	cases := []struct {
		name     string
		build    Build
		consumer string
		since    CommitsSince
	}{
		{"equal to or ahead of main", current, harness, mainLacking(0, nil, nil)},
		{"a non-harness Consumer", current, "example.com/shop", mainLacking(5, nil, nil)},
		{"a Consumer with no go.mod", current, "", mainLacking(5, nil, nil)},
		{"a module path that only starts with the harness's", current, harness + "-fork", mainLacking(5, nil, nil)},
		{"a binary with no recorded commit", Build{Module: harness}, harness, mainLacking(5, nil, nil)},
		{"a binary with no module path", Build{Revision: current.Revision}, harness, mainLacking(5, nil, nil)},
		{"a count that could not run", current, harness, mainLacking(0, errors.New("bad revision"), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Stale(tc.build, checkoutOf(t, tc.consumer), tc.since); got != "" {
				t.Errorf("Stale = %q; want no warning", got)
			}
		})
	}
}

// The running binary names its own module, as the Consumer's go.mod would.
func TestRunningNamesTheHarnessModule(t *testing.T) {
	if got := Running().Module; got != harness {
		t.Errorf("Running().Module = %q; want %q", got, harness)
	}
}
