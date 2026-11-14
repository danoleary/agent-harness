package stages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
)

// ResolvedMarker is the file an implementation session writes into its findings
// dropbox (mounted at /findings) when it finds the ticket already fixed on
// origin/main and stops without committing (#329). Its shape is
// {"commit": "<sha>", "reason": "<why that commit fixes the ticket>"}.
const ResolvedMarker = "resolved.json"

// resolvedReport is the session's resolved marker as it wrote it.
type resolvedReport struct {
	Commit string `json:"commit"`
	Reason string `json:"reason"`
}

// clearResolved removes a resolved marker an earlier run of the ticket left in
// its findings dir, which is keyed on the ticket, not the run, so the stale
// marker cannot resolve this run. An absent marker is a no-op.
func clearResolved(findingsDir string) error {
	if err := os.Remove(filepath.Join(findingsDir, ResolvedMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// resolvedOnMain reads the session's resolved marker and checks it against the
// ground truth: origin/main must contain the commit it names. ok is false when
// there is no marker. When there is one, err says why it does not hold, and is
// nil when it does: the Result then carries ResolvedOnMain, the commit's full
// SHA and the session's reason.
func resolvedOnMain(wt gitpkg.Worktree, findingsDir string) (res Result, ok bool, err error) {
	raw, rErr := os.ReadFile(filepath.Join(findingsDir, ResolvedMarker))
	if errors.Is(rErr, os.ErrNotExist) {
		return Result{}, false, nil
	}
	if rErr != nil {
		return Result{}, true, rErr
	}
	var rep resolvedReport
	if jErr := json.Unmarshal(raw, &rep); jErr != nil {
		return Result{}, true, fmt.Errorf("it is not {commit, reason} JSON: %w", jErr)
	}
	commit := strings.TrimSpace(rep.Commit)
	sha, onBase := wt.OnBase(commit)
	if !onBase {
		return Result{}, true, fmt.Errorf("its commit %q is not on %s", commit, gitpkg.BaseRef)
	}
	return Result{Disposition: ResolvedOnMain, ResolvedBy: sha, ResolvedWhy: strings.TrimSpace(rep.Reason)}, true, nil
}
