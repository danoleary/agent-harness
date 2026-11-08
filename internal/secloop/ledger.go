package secloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LedgerFile is the attempt ledger's name under the loop's logs dir. An attempt
// that pushed nothing has no PR to carry its marker, so the ledger is where the
// budget is kept (ADR-0040, decision 6).
const LedgerFile = "attempts.jsonl"

// The outcomes a ledger line records.
const (
	// OutcomeStarted is a run holding the lease on an item: one attempt.
	OutcomeStarted = "started"
	// OutcomeHandedOff is the budget on a fingerprint running out: the loop leaves
	// the package to a human until its alerts change.
	OutcomeHandedOff = "handed_off"
	// OutcomeResolved is a fix whose replay onto origin/main collapsed to nothing:
	// main already fixed the package, so the loop waits for Dependabot to close
	// the alerts rather than retrying.
	OutcomeResolved = "resolved"
)

// Entry is one ledger line.
type Entry struct {
	Key     string    `json:"key"`
	FP      string    `json:"fp"`
	BaseSHA string    `json:"base_sha"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
}

// readLedger reads every line of the ledger at path; a ledger not yet written is
// empty. A line that does not parse is an error: the budget it counts would be
// unknown.
func readLedger(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// appendLedger appends one line to the ledger at path, creating it and its dir.
func appendLedger(path string, e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// tally is what the ledger says about one fingerprint of one Key.
type tally struct {
	// attempts counts the started lines.
	attempts int
	// handedOff marks a fingerprint whose budget ran out and was handed to a human.
	handedOff bool
	// bases are the origin/main SHAs an attempt started at.
	bases map[string]bool
	// resolved marks a fingerprint main already fixed, as of resolvedAt.
	resolved   bool
	resolvedAt time.Time
}

func tallyOf(entries []Entry, key, fp string) tally {
	t := tally{bases: map[string]bool{}}
	for _, e := range entries {
		if e.Key != key || e.FP != fp {
			continue
		}
		switch e.Outcome {
		case OutcomeStarted:
			t.attempts++
			t.bases[e.BaseSHA] = true
		case OutcomeHandedOff:
			t.handedOff = true
		case OutcomeResolved:
			t.resolved = true
			if e.At.After(t.resolvedAt) {
				t.resolvedAt = e.At
			}
		}
	}
	return t
}
