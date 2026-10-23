package git

import (
	"os"
	"path/filepath"
	"testing"
)

// BEH-528: the dispatch guard's matcher must recognise a ticket key already
// referenced in recent main history — the canonical shape is the trailing
// `(BEH-NNN)` a squash-merge subject carries.
func TestMainHistoryReferencesMatchesParenthesisedKey(t *testing.T) {
	log := "docs(harness): document shared-DB isolation rules (BEH-521) (#542)\n" +
		"refactor(call): remove dead props (BEH-436) (#538)\n"
	if !mainHistoryReferences(log, "BEH-521") {
		t.Fatalf("expected BEH-521 to be found in recent main history:\n%s", log)
	}
}

// An unrelated history must not match — the guard only skips a ticket whose work
// has actually merged, never one that simply hasn't been touched.
func TestMainHistoryReferencesNoMatchWhenAbsent(t *testing.T) {
	log := "refactor(call): remove dead props (BEH-436) (#538)\n"
	if mainHistoryReferences(log, "BEH-521") {
		t.Fatalf("BEH-521 is absent and must not match:\n%s", log)
	}
}

// Word boundaries, both directions: a shorter key is not a prefix-match of a
// longer one, and a longer key is not matched by a shorter merged commit. A
// substring grep (the finding's literal proposal) would get both of these wrong
// and wrongly skip an unmerged ticket.
func TestMainHistoryReferencesRespectsWordBoundaries(t *testing.T) {
	log := "docs(harness): document shared-DB isolation rules (BEH-521) (#542)\n"
	if mainHistoryReferences(log, "BEH-52") {
		t.Fatalf("BEH-52 must not match the longer BEH-521")
	}
	if mainHistoryReferences("fix: thing (BEH-5210)\n", "BEH-521") {
		t.Fatalf("BEH-521 must not match the longer BEH-5210")
	}
}

// BEH-633: the Key is tracker-agnostic (ADR-0010) — the history scan must make no
// `BEH-` assumption. A Jira-style `PROJ-123` matches when merged and still
// respects word boundaries (PROJ-12 must not match the longer PROJ-123), exactly
// as a Linear key does, so a future Jira adapter dispatches correctly.
func TestMainHistoryReferencesMatchesNonBEHKey(t *testing.T) {
	log := "feat(auth): add SSO callback (PROJ-123) (#88)\n"
	if !mainHistoryReferences(log, "PROJ-123") {
		t.Fatalf("expected the non-BEH Key PROJ-123 to match:\n%s", log)
	}
	if mainHistoryReferences(log, "PROJ-12") {
		t.Fatalf("PROJ-12 must not match the longer PROJ-123 — word boundaries are Key-agnostic")
	}
}

// Subjects occasionally lower-case the key (and the identifier always arrives
// upper-cased from arg parsing); the match must be case-insensitive so a real
// merge isn't missed on casing alone.
func TestMainHistoryReferencesIsCaseInsensitive(t *testing.T) {
	if !mainHistoryReferences("fix: thing (beh-521)\n", "BEH-521") {
		t.Fatalf("expected a case-insensitive match against beh-521")
	}
}

// #36: a GitHub Key starts with `#`, a non-word character, so a `\b` before it
// never fires on a real reference like "(#30)" or "closes #30". The Key must be
// matched on "not flanked by a word character" instead, which still keeps a
// shorter Key from matching a longer one.
func TestKeyReferencedMatchesGitHubKey(t *testing.T) {
	cases := []struct {
		text, key string
		want      bool
	}{
		{"Fix foo (#30)", "#30", true},
		{"closes #30", "#30", true},
		{"#30: title", "#30", true},
		{"fix: thing\n#30 at the start of a line", "#30", true},
		{"Fix foo (#300)", "#30", false},
		{"a#30b", "#30", false},
		{"Fix foo (#30)", "#3", false},
		{"(BEH-521)", "BEH-52", false},
		{"(BEH-52)", "BEH-52", true},
	}
	for _, c := range cases {
		if got := keyReferenced(c.text, c.key); got != c.want {
			t.Errorf("keyReferenced(%q, %q) = %v, want %v", c.text, c.key, got, c.want)
		}
	}
}

// commitAndPush makes a commit with the given subject in dir and pushes main to
// origin — modelling an author landing a PR on the shared remote.
func commitAndPush(t *testing.T, dir, subject string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(subject), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", subject)
	runGit(t, dir, "push", "-q", "origin", "main")
}

// newSharedRemote returns (origin, host) where origin is a bare repo seeded with
// one commit and host is a clone of it — the shape of the harness's primary
// checkout against the team remote, with the host's origin/main intentionally
// behind whatever lands on origin next.
func newSharedRemote(t *testing.T) (string, string) {
	t.Helper()
	origin := t.TempDir()
	runGit(t, origin, "init", "-q", "--bare", "-b", "main")

	author := t.TempDir()
	runGit(t, author, "clone", "-q", origin, ".")
	runGit(t, author, "config", "user.email", "test@example.com")
	runGit(t, author, "config", "user.name", "Test")
	commitAndPush(t, author, "chore: init")

	host := t.TempDir()
	runGit(t, host, "clone", "-q", origin, ".")
	return author, host
}

// BEH-528 (the whole point): a ticket whose work already merged on the shared
// remote must be reported as on-main even when the host checkout's origin/main
// is stale — the guard's own fetch refreshes it. Asserting against a stale host
// proves the fetch is load-bearing, not decorative.
func TestTicketAlreadyOnMainFetchesThenFindsMergedTicket(t *testing.T) {
	author, host := newSharedRemote(t)

	commitAndPush(t, author, "docs(harness): isolation rules (BEH-521) (#542)")

	if !Open(host, testPrefix).TicketAlreadyOnMain("BEH-521") {
		t.Fatalf("BEH-521 merged on the remote — the guard should fetch and find it")
	}
}

// The negative the dispatch path depends on: a ticket with no merged commit must
// not be reported as on-main, or every fresh ticket would be wrongly skipped.
func TestTicketAlreadyOnMainFalseWhenUnmerged(t *testing.T) {
	author, host := newSharedRemote(t)

	commitAndPush(t, author, "docs(harness): isolation rules (BEH-521) (#542)")

	if Open(host, testPrefix).TicketAlreadyOnMain("BEH-999") {
		t.Fatalf("BEH-999 never merged — dispatch must not be skipped")
	}
}

// A git failure (no origin remote, corrupt checkout) must fail open: return false
// so a flaky read can never block a legitimate dispatch. The cost of a false
// negative is one session — exactly the pre-guard status quo — whereas a false
// positive silently drops real work.
func TestTicketAlreadyOnMainFailsOpenWhenGitErrors(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init (BEH-521)")

	// No origin remote and no origin/main ref — the log read fails.
	if Open(repo, testPrefix).TicketAlreadyOnMain("BEH-521") {
		t.Fatalf("a git failure must fail open (false), never block dispatch")
	}
}
