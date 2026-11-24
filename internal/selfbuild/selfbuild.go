// Package selfbuild tells the operator when the harness is working on its own
// repo with a binary older than the harness changes already on origin/main
// (#461).
//
// A fix to dispatch or selection merged to main only takes effect once someone
// rebuilds the binary. Until then the loop runs the old behaviour, every move of
// main can repeat the run the fix was meant to stop, and a retrospective may file
// the same class again. Nothing in the run said the binary was behind.
package selfbuild

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/danoleary/agent-harness/internal/version"
)

// Build is the running binary: the Go module it was built from and the commit.
type Build struct {
	Module   string
	Revision string
}

// Running is the binary this process runs.
func Running() Build {
	b := Build{Revision: version.Revision()}
	if info, ok := debug.ReadBuildInfo(); ok {
		b.Module = info.Main.Path
	}
	return b
}

// CommitsSince counts the commits origin/main has and commit lacks, among those
// touching paths. [git.Checkout.MainCommitsSince] is the real one.
type CommitsSince func(commit string, paths ...string) (int, error)

// harnessPaths are the directories whose commits change what the binary does.
var harnessPaths = []string{"internal", "cmd"}

// Stale is the warning to log when the Consumer checkout at dir is the repo b
// was built from and origin/main has harness commits b lacks, or "" otherwise.
// It is a warning, never a stop: an older binary still works. A Consumer that is
// not this module, a binary that cannot say what it was built from, and a count
// that cannot run (a build commit the checkout never fetched) all stay quiet.
func Stale(b Build, dir string, since CommitsSince) string {
	if b.Module == "" || b.Revision == "" || moduleOf(dir) != b.Module {
		return ""
	}
	n, err := since(b.Revision, harnessPaths...)
	if err != nil || n <= 0 {
		return ""
	}
	commits := "commit"
	if n != 1 {
		commits = "commits"
	}
	return fmt.Sprintf("⚠ this agent-harness binary was built from %s, which lacks %d harness %s on origin/main (under internal/ or cmd/): a fix merged there is not running. Rebuild (make build) and restart the loop (#461)", short(b.Revision), n, commits)
}

// moduleOf is the module path dir's go.mod declares, or "" without one.
func moduleOf(dir string) string {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// short abbreviates a commit SHA for narration.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
