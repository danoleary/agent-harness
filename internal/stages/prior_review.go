package stages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/ship"
)

// priorReviewFile is the per-ticket record of the last review's push decision — the
// HEAD it covered, the verdict, and the host Gate re-run over it — kept in the
// ticket's log dir so it outlives the run (#44, #230).
const priorReviewFile = "review-verdict.json"

type reviewRecord struct {
	Head        string `json:"head"`
	Disposition string `json:"disposition"`
	Blocked     bool   `json:"blocked,omitempty"`
	// Gate is the host Gate re-run over Head. A record written before #230 has none,
	// which reads back as no gate run rather than as a green one.
	Gate *gateRecord `json:"gate,omitempty"`
}

type gateRecord struct {
	Exit   int    `json:"exit"`
	Failed string `json:"failed,omitempty"`
}

// clearDisposition leads the only verdict worth carrying forward.
const clearDisposition = "Disposition: clear"

// readRecord reads the ticket's record, ok=false when it is missing or unreadable.
func readRecord(logDir string) (reviewRecord, bool) {
	raw, err := os.ReadFile(filepath.Join(logDir, priorReviewFile))
	if err != nil {
		return reviewRecord{}, false
	}
	var r reviewRecord
	if json.Unmarshal(raw, &r) != nil || r.Head == "" {
		return reviewRecord{}, false
	}
	return r, true
}

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
	r, ok := readRecord(logDir)
	if !ok || r.Head != head || !strings.HasPrefix(r.Disposition, clearDisposition) {
		return nil
	}
	return &prompt.PriorReview{Head: r.Head, Disposition: r.Disposition}
}

// LastPushGate is the review stage's last recorded push decision for the ticket
// whose log dir is logDir, nil when there is none. The committed-fix recovery reads
// it to tell a gate-green commit from one the review refused to ship (#230).
func LastPushGate(logDir string) *ship.PushGate {
	r, ok := readRecord(logDir)
	if !ok {
		return nil
	}
	g := &ship.PushGate{Head: r.Head, Disposition: r.Disposition, Blocked: r.Blocked}
	if r.Gate != nil {
		g.GateRan, g.GateExit, g.FailedGate = true, r.Gate.Exit, r.Gate.Failed
	}
	return g
}

// recordPushGate overwrites the ticket's record with the push decision over g.Head.
// A decision with no verdict keeps the verdict an earlier run recorded over the
// same HEAD: the commit it judged is unchanged, so a clear one is still worth
// handing a re-review (#44) and a blocked one still stops the recovery (#230).
func recordPushGate(logDir string, g ship.PushGate) error {
	r := reviewRecord{Head: g.Head, Disposition: g.Disposition, Blocked: g.Blocked}
	if g.GateRan {
		r.Gate = &gateRecord{Exit: g.GateExit, Failed: g.FailedGate}
	}
	if prior, ok := readRecord(logDir); ok && r.Disposition == "" && !r.Blocked && prior.Head == r.Head {
		r.Disposition, r.Blocked = prior.Disposition, prior.Blocked
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(logDir, priorReviewFile), raw, 0o644)
}
