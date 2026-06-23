package ci

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCIRerunTimeout is returned when, after pushing an auto-fix commit, CI never
// registers a run against the new branch HEAD within the poll budget. The fix was
// pushed but the harness cannot confirm CI re-evaluated it, so the watch stops
// rather than re-acting on the prior run's stale red (BEH-493).
var ErrCIRerunTimeout = errors.New("CI did not start a run for the pushed fix within the poll budget")

// runHeadJSONField is the single `gh run list --json` field awaitHeadRun reads.
const runHeadJSONField = "headSha"

// awaitHeadRun blocks until ciHead — the head commit of the latest CI run for the
// branch — equals localHead, the commit the just-pushed fix produced, i.e. CI has
// actually started evaluating the new HEAD. Until then the prior run's red is
// stale and must not trigger another fix: a fast fix commit routinely lands before
// CI re-runs, and an immediate re-poll would re-act on the predating run and burn a
// whole fix session against an already-fixed problem (BEH-493). It returns nil once
// they converge, or ErrCIRerunTimeout if CI never picks up the new HEAD within the
// budget. A ciHead fetch error aborts immediately (a real gh failure, not something
// more polling fixes). The clock is injected so the timing is deterministic in
// tests (mirrors poll).
func awaitHeadRun(localHead string, ciHead func() (string, error), cfg pollConfig, sleep func(time.Duration), now func() time.Time) error {
	deadline := now().Add(cfg.budget)
	for {
		head, err := ciHead()
		if err != nil {
			return err
		}
		if head == localHead {
			return nil
		}
		// Not caught up (stale prior run, or no run registered yet) — stop if the
		// next interval would carry us past the budget (no point sleeping toward a
		// deadline we can't beat), mirroring poll.
		if !now().Add(cfg.interval).Before(deadline) {
			return ErrCIRerunTimeout
		}
		sleep(cfg.interval)
	}
}

// interpretRunHead pulls the head commit SHA of the most recent CI run out of a
// `gh run list --branch <branch> -L 1 --json headSha` invocation. An empty array
// means Actions has not registered any run for the branch yet — it returns "" (a
// "keep waiting" signal, not an error). A gh failure with no parseable JSON is
// surfaced verbatim (a real failure — auth, bad branch — not something polling
// fixes). Mirrors interpretChecksOutput.
func interpretRunHead(stdout, stderr []byte, runErr error) (string, error) {
	var runs []struct {
		HeadSHA string `json:"headSha"`
	}
	if err := json.Unmarshal(stdout, &runs); err == nil {
		if len(runs) == 0 {
			return "", nil
		}
		return runs[0].HeadSHA, nil
	}
	if runErr != nil {
		s := strings.TrimSpace(string(stderr))
		if s != "" {
			return "", fmt.Errorf("gh run list failed: %w: %s", runErr, s)
		}
		return "", fmt.Errorf("gh run list failed: %w", runErr)
	}
	return "", fmt.Errorf("gh run list returned no parseable JSON: %q", strings.TrimSpace(string(stdout)))
}
