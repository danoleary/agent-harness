// Package stream turns claude's stream-json output lines into concise console
// narration.
package stream

import (
	"encoding/json"
	"fmt"
	"math"
)

type event struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	IsError    bool     `json:"is_error"`
	DurationMS *float64 `json:"duration_ms"`
	Message    struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// Narrate turns one line of claude's `--output-format stream-json` into a concise
// console narration string, returning ok=false to skip it. The full raw stream
// is teed to the per-run jsonl regardless; this is only the human-friendly
// summary shown when the harness is not running --verbose. Never panics — a
// malformed line is simply skipped so a single bad chunk can't kill the run.
func Narrate(line string) (string, bool) {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return "", false
	}

	if e.Type == "result" {
		mark := "✓"
		if e.IsError {
			mark = "✗"
		}
		secs := ""
		if e.DurationMS != nil {
			secs = fmt.Sprintf(" (%ds)", int(math.Round(*e.DurationMS/1000)))
		}
		subtype := e.Subtype
		if subtype == "" {
			subtype = "ended"
		}
		return fmt.Sprintf("%s session %s%s", mark, subtype, secs), true
	}

	if e.Type == "assistant" {
		for _, block := range e.Message.Content {
			if block.Type == "tool_use" && block.Name != "" {
				return "⚒ " + block.Name, true
			}
		}
	}

	return "", false
}
