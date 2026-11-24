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

// interactiveGitAwk finds, in a Bash command on stdin, the first simple command
// that runs a git mode which prompts on stdin, and prints it (`git add -p`). It
// splits the command as a shell would, near enough: quotes, backslashes,
// comments, heredoc bodies and `;&|()` separators. So a commit message or a grep
// pattern that names `git add -p` is not a hit, and `git checkout -- -p` names a
// path, not a flag. It exits 1 when there is no hit. No single quote anywhere:
// it runs inside one in sh.
const interactiveGitAwk = `
function flush() { if (inw) words[++n] = w; w = ""; inw = 0 }
function seg(   k, g, sc, j, f) {
  flush()
  k = 1
  while (k <= n && (words[k] ~ /^[A-Za-z_][A-Za-z0-9_]*=/ || index(" env command exec nohup time sudo if elif while until then do else ! { ", " " words[k] " "))) k++
  if (words[k] == "timeout") { k++; while (k <= n && words[k] !~ /^[0-9.]+[smhd]?$/) k++; k++ }
  g = words[k]; sub(/.*\//, "", g)
  if (g == "git" && hit == "") {
    k++
    while (k <= n && words[k] ~ /^-/) { if (words[k] == "-C" || words[k] == "-c") k++; k++ }
    sc = words[k]; k++
    if (sc == "stash" && k <= n && words[k] !~ /^-/ && words[k] != "push" && words[k] != "save") sc = ""
    for (j = k; j <= n && words[j] != "--"; j++) {
      f = words[j]
      if (((f == "-p" || f == "--patch") && index(" add checkout restore reset stash commit ", " " sc " ")) ||
          (f == "-i" && index(" add clean rebase ", " " sc " ")) ||
          (f == "--interactive" && index(" add clean rebase commit ", " " sc " "))) { hit = "git " sc " " f; break }
    }
  }
  n = 0
}
{ s = s $0 "\n" }
END {
  sq = "\047"; dq = "\""; L = length(s); i = 1; q = ""; nd = 0
  while (i <= L) {
    c = substr(s, i, 1)
    if (q != "") {
      if (c == q) q = ""
      else if (q == dq && c == "\\") { i++; w = w substr(s, i, 1) }
      else w = w c
      i++; continue
    }
    if (c == "\\") { if (substr(s, i + 1, 1) != "\n") { w = w substr(s, i + 1, 1); inw = 1 } i += 2; continue }
    if (c == sq || c == dq) { q = c; inw = 1; i++; continue }
    if (c == "#" && !inw) { while (i <= L && substr(s, i, 1) != "\n") i++; continue }
    if (substr(s, i, 3) == "<<<") { flush(); i += 3; continue }
    if (substr(s, i, 2) == "<<") {
      flush(); i += 2
      if (substr(s, i, 1) == "-") i++
      while (substr(s, i, 1) == " " || substr(s, i, 1) == "\t") i++
      d = ""
      while (i <= L) { c = substr(s, i, 1); if (index(" \t\n;&|<>()", c)) break; if (c != sq && c != dq && c != "\\") d = d c; i++ }
      if (d != "") delim[++nd] = d
      continue
    }
    if (c == "\n") {
      seg(); i++
      for (h = 1; h <= nd; h++) {
        while (i <= L) {
          e = index(substr(s, i), "\n"); line = substr(s, i, e - 1); i += e
          sub(/^\t+/, "", line)
          if (line == delim[h]) break
        }
      }
      nd = 0; continue
    }
    if (index(";&|()", c)) { seg(); i++; continue }
    if (c == " " || c == "\t") { flush(); i++; continue }
    w = w c; inw = 1; i++
  }
  seg()
  if (hit == "") exit 1
  print hit
}
`

// interactiveGitGuardScript is the guard that refuses a Bash command running a
// git mode that prompts: add, checkout, restore, reset, stash or commit with
// -p/--patch, add, clean or rebase with -i, and --interactive (#353). The prompt
// waits on a stdin the Bash tool never answers, so the call hangs until the Bash
// timeout, which the sandbox raises to 10 minutes. Exit 2 blocks the call and
// hands stderr to the session, so it learns at once and can rerun the
// non-interactive form. diff, log and show -p print and never prompt, so they
// pass. An awk that fails lets the call through: the guard only saves time.
//
// The checkout and restore advice warns that they drop every uncommitted change
// to the path: a session reaches for checkout -p to drop only part of its work
// (#433).
func interactiveGitGuardScript() string {
	return strings.Join([]string{
		`hit=$(jq -r '.tool_input.command // empty' | awk '` + interactiveGitAwk + `') || exit 0`,
		`echo "Blocked: $hit is an interactive git mode. It waits on a prompt this sandbox's Bash never answers, so the call would hang until its timeout. Use the non-interactive form instead: git add <path> to stage a file, git stash push -- <path>, or edit the file. git checkout -- <path> and git restore <path> discard every uncommitted change to the path, not one hunk: to undo a temporary edit, copy the file aside first and copy it back, or edit the file." >&2`,
		`exit 2`,
	}, "\n")
}

// guardDir is where a sandboxed session's SessionStart hook installs the
// interactive-git guard script.
const guardDir = "/tmp/agent-harness"

// guardScriptPath is the guard script's path under dir.
func guardScriptPath(dir string) string { return dir + "/interactive-git-guard.sh" }

// installGuardCommand is the SessionStart hook that writes the guard script into
// dir, before the session's first tool call. The guard is installed as a file
// rather than inlined in its PreToolUse hook because the CLI prefixes a block's
// reason with the whole hook command: an inline guard handed the session 3 KB of
// awk before the one line it needed (#433). Written aside and moved, so a hook
// never runs a half-written script.
func installGuardCommand(dir string) string {
	const eof = "AGENT_HARNESS_GUARD_EOF"
	p := guardScriptPath(dir)
	return "mkdir -p " + dir + " && cat > " + p + ".tmp <<'" + eof + "' && mv " + p + ".tmp " + p + "\n" +
		interactiveGitGuardScript() + "\n" + eof
}

// interactiveGitGuardCommand is the PreToolUse hook that runs the guard script
// installGuardCommand wrote into dir. One short line, since the CLI echoes it
// with every block. A missing script lets the call through, as a failing awk does.
func interactiveGitGuardCommand(dir string) string {
	p := guardScriptPath(dir)
	return "[ -f " + p + " ] || exit 0; exec sh " + p
}

// sessionSettings is the `--settings` JSON every sandboxed session starts with.
// It travels on the command line rather than in the image so a change reaches
// the next session without a new base image release, and it adds to the
// Consumer's own project settings rather than replacing them. A copy is kept in
// testdata/session-settings.json, so a session can run the pinned CLI on it
// (docs/how-to/sandbox-image.md; #437).
func sessionSettings() string { return sessionSettingsIn(guardDir) }

// sessionSettingsIn is sessionSettings with the guard script installed in dir.
func sessionSettingsIn(dir string) string {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	raw, err := json.Marshal(map[string]any{
		"hooks": map[string][]matcher{
			"SessionStart": {{Hooks: []hook{
				{Type: "command", Command: installGuardCommand(dir), Timeout: 30},
			}}},
			"PreToolUse": {{Matcher: "Bash", Hooks: []hook{
				{Type: "command", Command: reapLeftoversCommand("/proc"), Timeout: 30},
				{Type: "command", Command: interactiveGitGuardCommand(dir), Timeout: 30},
			}}},
		},
	})
	if err != nil {
		panic(err) // a static value of plain strings always marshals
	}
	return string(raw)
}
