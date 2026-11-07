package sandbox

import (
	"encoding/json"
	"strings"
)

// storyRunPattern is what marks a Bash command as a story run: it invokes vitest
// or Storybook. The reaper runs before those and no others.
const storyRunPattern = "vitest|storybook"

// reapLeftoversCommand is the PreToolUse hook that kills any headless browser and
// vitest process left in the container before a story run starts (#248). An
// outer `timeout` around a hung story run kills the package manager but not the
// browser it launched, and the browser left behind slows or blocks the next run
// (ADR-0044).
//
// It reads each process's argv from procRoot ("/proc" in the container) rather
// than calling pkill: the base image does not promise procps, and `pkill -f`
// would also match the claude process, whose argv carries a prompt that names
// both. Only argv[0] and argv[1] are read, so a process is matched by what it
// runs, not by what its arguments mention. POSIX sh, because Claude Code runs a
// hook command through a shell it does not let the harness pick.
func reapLeftoversCommand(procRoot string) string {
	return strings.Join([]string{
		`jq -r '.tool_input.command // empty' | grep -Eq '` + storyRunPattern + `' || exit 0`,
		`for d in ` + procRoot + `/[0-9]*; do`,
		`  tr '\000' '\n' < "$d/cmdline" 2>/dev/null | head -n 2 | awk 'NR == 1 { n = $0; sub(/.*\//, "", n); if (n == "chrome-headless-shell" || n == "headless_shell") hit = 1 } /vitest/ { hit = 1 } END { exit !hit }' && kill -9 "${d##*/}" 2>/dev/null`,
		`done`,
		`exit 0`,
	}, "\n")
}

// sessionSettings is the `--settings` JSON every sandboxed session starts with.
// It travels on the command line rather than in the image so a change reaches
// the next session without a new base image release, and it adds to the
// Consumer's own project settings rather than replacing them.
func sessionSettings() string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	}
	raw, err := json.Marshal(map[string]any{
		"hooks": map[string][]matcher{
			"PreToolUse": {{Matcher: "Bash", Hooks: []hook{{Type: "command", Command: reapLeftoversCommand("/proc"), Timeout: 30}}}},
		},
	})
	if err != nil {
		panic(err) // a static value of plain strings always marshals
	}
	return string(raw)
}
