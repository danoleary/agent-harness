// Package trackers wires the concrete tracker adapters to the host-side tracker
// port and selects one by the Consumer's configured kind (ADR-0010: the port has
// Linear/Jira/GitHub adapters, selected by `.agent-harness/config`). It is the one
// place adapter selection happens, so the five construction sites (the three
// stages plus cmd/loop and cmd/pipeline) share it and a future adapter plugs in
// here, not at every call site.
//
// It sits above internal/tracker (the port) and internal/linear (the first
// adapter): the port must not import an adapter, so the selection lives here.
package trackers

import (
	"fmt"

	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/tracker"
)

// New returns the tracker adapter for kind, holding the host-only credential
// (ADR-0001: the key stays on the host, never crosses the sandbox boundary). An
// unsupported kind is a loud error — the harness refuses to run against a tracker
// it has no adapter for. Jira and GitHub Issues land in BEH-637/638.
func New(kind, apiKey string) (tracker.Tracker, error) {
	switch kind {
	case "linear":
		return linear.NewClient(linear.NewTransport(apiKey)), nil
	default:
		return nil, fmt.Errorf("unsupported tracker kind %q (supported: linear)", kind)
	}
}
