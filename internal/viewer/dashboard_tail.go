package viewer

import "strings"

// DashboardTailer follows the loop.jsonl stream and folds newly-appended records
// into a Dashboard across repeated Poll calls — the live-view counterpart to
// Tailer, sharing the same stdlib follow engine. The command loops Poll on a
// ticker and renders Dashboard() each tick; the time/loop concern stays in the
// thin command, keeping this unit-testable.
type DashboardTailer struct {
	follow follower
	dash   *Dashboard
}

// NewDashboardTailer returns a DashboardTailer over the stream file at path.
func NewDashboardTailer(path string) *DashboardTailer {
	return &DashboardTailer{follow: follower{path: path}, dash: NewDashboard()}
}

// Poll folds any complete records appended since the last Poll into the model.
// running=false means the file is absent. A truncation (daemon restart) resets the
// model so the fresh run renders from its start.
func (t *DashboardTailer) Poll() (running bool, err error) {
	data, running, reset, err := t.follow.next()
	if reset {
		t.dash = NewDashboard()
	}
	if err != nil || !running || len(data) == 0 {
		return running, err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if r, ok := parse(line); ok {
			t.dash.Observe(r)
		}
	}
	return true, nil
}

// Dashboard is the model the tailer folds records into — the command renders it.
func (t *DashboardTailer) Dashboard() *Dashboard { return t.dash }
