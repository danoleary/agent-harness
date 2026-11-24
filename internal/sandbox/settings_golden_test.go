package sandbox

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites the golden files this package keeps instead of checking them.
var update = flag.Bool("update", false, "rewrite internal/sandbox/testdata golden files")

// sessionSettingsGolden is the `--settings` JSON every sandboxed session starts
// with, kept on disk so a session checking a hook change against the pinned CLI
// can `cat` it rather than write a scratch test into this package (#437).
const sessionSettingsGolden = "testdata/session-settings.json"

// #437: testdata/session-settings.json is the exact --settings value the
// launcher passes, one line, so `claude --settings "$(cat …)"` runs the hooks a
// session gets. A hook change that leaves it stale fails here; -update rewrites it.
func TestTheSessionSettingsGoldenIsTheLaunchersSettings(t *testing.T) {
	want := sessionSettings() + "\n"
	if *update {
		if err := os.MkdirAll(filepath.Dir(sessionSettingsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionSettingsGolden, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(sessionSettingsGolden)
	if err != nil {
		t.Fatalf("read the golden: %v (run `go test ./internal/sandbox/ -run %s -update` to write it)", err, t.Name())
	}
	if string(got) != want {
		t.Errorf("%s is stale: the session settings changed. Run `go test ./internal/sandbox/ -run %s -update` and commit the result.\ngot:  %s\nwant: %s",
			sessionSettingsGolden, t.Name(), got, want)
	}
}
