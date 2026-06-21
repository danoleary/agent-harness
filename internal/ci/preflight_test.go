package ci

import (
	"errors"
	"strings"
	"testing"
)

func TestChecksReadableTrueWhenProbeSucceeds(t *testing.T) {
	// A token that can read the Checks API → the probe exits clean → readable.
	ok, detail := ChecksReadable(func(string, ...string) ([]byte, error) {
		return []byte(`{"total_count":0,"check_runs":[]}`), nil
	})
	if !ok {
		t.Fatalf("expected readable, got false (%q)", detail)
	}
}

func TestChecksReadableFalseOnPermissionDenied(t *testing.T) {
	// A fine-grained PAT 403s on the Checks API. The probe must flag it unreadable
	// with an actionable message naming the classic repo-scoped PAT fix.
	ok, detail := ChecksReadable(func(string, ...string) ([]byte, error) {
		return []byte("gh: Resource not accessible by personal access token (HTTP 403)"),
			errors.New("exit status 1")
	})
	if ok {
		t.Fatal("expected unreadable on a 403 permission error")
	}
	if !strings.Contains(detail, "classic") || !strings.Contains(strings.ToLower(detail), "check runs") {
		t.Fatalf("detail %q should name the classic repo-scoped PAT fix", detail)
	}
}

func TestChecksReadableTrueOnUnrelatedError(t *testing.T) {
	// A transient/unrelated probe failure (network, rate limit) is NOT a permission
	// ceiling — don't block the run on a flaky probe; the watch's degrade path is
	// the real safety net.
	ok, _ := ChecksReadable(func(string, ...string) ([]byte, error) {
		return []byte("dial tcp: lookup api.github.com: no such host"), errors.New("exit status 1")
	})
	if !ok {
		t.Fatal("an unrelated probe error must not be treated as a permission ceiling")
	}
}

func TestChecksReadableProbesCheckRunsEndpoint(t *testing.T) {
	// The probe must hit the Checks API (commits/<ref>/check-runs) — the exact
	// resource a fine-grained PAT is blind to — not some other endpoint that a
	// fine-grained PAT *can* read (which would pass a blind token).
	var gotArgs []string
	ChecksReadable(func(name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return nil, nil
	})
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "check-runs") {
		t.Fatalf("probe args %q must target the check-runs endpoint", joined)
	}
}
