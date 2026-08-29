package viewer

import (
	"encoding/json"

	"github.com/danoleary/agent-harness/internal/loopstream"
)

// parse decodes one JSONL line into a Record. ok=false on a malformed line.
func parse(line string) (loopstream.Record, bool) {
	var r loopstream.Record
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return loopstream.Record{}, false
	}
	return r, true
}
