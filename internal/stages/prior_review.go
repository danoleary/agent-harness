package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/prompt"
)

// priorReviewFile is the per-ticket record of the last review's verdict and the
// HEAD it covered, kept in the ticket's log dir so it outlives the run (#44).
const priorReviewFile = "review-verdict.json"

type reviewRecord struct {
	Head        string `json:"head"`
	Disposition string `json:"disposition"`
}

// clearDisposition leads the only verdict worth carrying forward.
const clearDisposition = "Disposition: clear"

// readPriorReview returns the recorded verdict when it was clear and covered
// exactly head — the branch has gained no commit since that review — and nil
// otherwise, so a moved branch, a missing or unreadable record, or an unknown HEAD
// all review cold. A blocked verdict is never carried forward: it waits on a human
// decision that lands on the ticket, not the branch, so re-emitting it over the
// same HEAD would keep the ticket blocked after the human answered.
func readPriorReview(logDir, head string) *prompt.PriorReview {
	if head == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(logDir, priorReviewFile))
	if err != nil {
		return nil
	}
	var r reviewRecord
	if json.Unmarshal(raw, &r) != nil || r.Head != head || !strings.HasPrefix(r.Disposition, clearDisposition) {
		return nil
	}
	return &prompt.PriorReview{Head: r.Head, Disposition: r.Disposition}
}

// recordReview overwrites the ticket's record with the verdict that covered head.
func recordReview(logDir, head, disposition string) error {
	raw, err := json.Marshal(reviewRecord{Head: head, Disposition: disposition})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(logDir, priorReviewFile), raw, 0o644)
}
