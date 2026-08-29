package stages

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
)

// Every runtime artifact the harness writes into a Consumer checkout must land
// under the ONE directory that Consumer opted into by creating — `.agent-harness/`
// (ADR-0008). Logs used to go to `<checkout>/agent-harness/logs`, the harness's own
// in-tree source directory. That only held while the harness WAS a subdirectory of
// its single Consumer; once it is extracted (ADR-0007) that path names a directory
// the Consumer does not have, so the harness would silently create a phantom
// source-looking `agent-harness/` inside every project it runs against — untracked,
// un-gitignored, and indistinguishable from a vendored copy of the harness itself.
func TestLogsRootLivesInTheConsumerHarnessDir(t *testing.T) {
	cfg := config.Config{ProjectPath: "/Users/dan/myproject"}

	got := LogsRoot(cfg)
	want := filepath.Join("/Users/dan/myproject", config.ProjectDirName, "logs")
	if got != want {
		t.Errorf("LogsRoot = %q, want %q", got, want)
	}
}

// The bare `agent-harness/` segment is the specific regression: it is the harness's
// own source directory name, and writing it into a Consumer checkout is what the
// extraction had to stop. Asserted separately from the exact path so the intent
// survives a future change to the directory's layout.
func TestLogsRootNeverWritesAHarnessSourceDirIntoAConsumer(t *testing.T) {
	got := LogsRoot(config.Config{ProjectPath: "/Users/dan/myproject"})

	for _, segment := range strings.Split(filepath.ToSlash(got), "/") {
		if segment == "agent-harness" {
			t.Errorf(
				"LogsRoot = %q writes a bare %q directory into the Consumer checkout; runtime "+
					"artifacts belong under %q, the directory the Consumer created (ADR-0008)",
				got, "agent-harness", config.ProjectDirName,
			)
		}
	}
}
