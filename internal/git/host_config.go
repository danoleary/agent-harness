package git

import (
	"bytes"
	"fmt"
	"strings"
)

// refuseRepoCommands is the check every host-side git command passes first (#351,
// ADR-0058). A session can write .git/config, and git runs the commands some keys
// name: a filter driver's smudge on `worktree add` and `reset --hard`, a merge
// driver on `cherry-pick`, diff.external on `diff`, core.sshCommand and a
// credential helper on `fetch` and `push`. A fixed -c list cannot override them,
// since a filter or driver name is the session's to pick, so the host lists the
// config git would read for args — `git [-C dir]... config --list` with
// includes — and refuses while a scope a session can write, local or worktree,
// sets one of [commandValued] to a command. A key set in global or system config
// is the operator's, and runs.
//
// The listing runs nothing a config names. If it fails, the command runs as
// asked: whatever stops git listing its config (a missing directory, a config it
// cannot parse) stops the command reading it too, and the caller gets git's own
// error rather than one from here.
func refuseRepoCommands(output outputRunner, args []string) error {
	out, err := output("git", append(dirArgs(args), "config", "--list", "--includes", "--show-scope", "--show-origin", "-z")...)
	if err != nil {
		return nil
	}
	for _, e := range parseConfigList(out) {
		if (e.scope == "local" || e.scope == "worktree") && e.value != "" && commandValued(e.key, e.value) {
			return fmt.Errorf("host git refused: %s sets %s, a command a sandbox session can write and host git would run (ADR-0058); "+
				"remove it from that file, or set it in your global git config", strings.TrimPrefix(e.origin, "file:"), e.key)
		}
	}
	return nil
}

// dirArgs are the -C options among args' leading global options, in order, so
// the listing reads the config of the directory the command will run in.
func dirArgs(args []string) []string {
	var dirs []string
	for i := 0; i+1 < len(args) && strings.HasPrefix(args[i], "-"); i += 2 {
		if args[i] == "-C" {
			dirs = append(dirs, "-C", args[i+1])
		} else if args[i] != "-c" {
			break
		}
	}
	return dirs
}

type configEntry struct{ scope, origin, key, value string }

// parseConfigList reads `git config --list --show-scope --show-origin -z`:
// scope NUL origin NUL key, then a newline and the value for a key that has one,
// NUL.
func parseConfigList(out []byte) []configEntry {
	fields := bytes.Split(out, []byte{0})
	var entries []configEntry
	for i := 0; i+2 < len(fields); i += 3 {
		key, value, _ := strings.Cut(string(fields[i+2]), "\n")
		entries = append(entries, configEntry{scope: string(fields[i]), origin: string(fields[i+1]), key: key, value: value})
	}
	return entries
}

// commandValued reports whether git runs key's value as a command in an operation
// the host performs. git lists a key with its section and variable lowercased and
// any subsection (a filter's, a driver's, a URL) as written. core.hooksPath and
// core.fsmonitor are not here: [hostSafeConfig] overrides them. A hook set in
// config, hook.<name>.command, is: core.hooksPath does not reach it.
func commandValued(key, value string) bool {
	section, rest, ok := strings.Cut(key, ".")
	if !ok {
		return false
	}
	variable := rest
	sub := ""
	if i := strings.LastIndex(rest, "."); i >= 0 {
		sub, variable = rest[:i], rest[i+1:]
	}
	switch section {
	case "filter":
		return sub != "" && (variable == "clean" || variable == "smudge" || variable == "process")
	case "diff":
		return (sub == "" && variable == "external") || (sub != "" && (variable == "command" || variable == "textconv"))
	case "merge":
		return sub != "" && variable == "driver"
	case "core":
		switch variable {
		case "sshcommand", "pager", "editor", "askpass", "gitproxy", "alternaterefscommand":
			return sub == ""
		}
	case "pager":
		// pager.<cmd> also takes a boolean, `pager.branch = false` the common one,
		// and a boolean names no command: true means core.pager, checked itself.
		return sub == "" && !gitBool(value)
	case "hook":
		return sub != "" && variable == "command"
	case "sequence":
		return sub == "" && variable == "editor"
	case "credential":
		return variable == "helper"
	case "remote":
		return sub != "" && (variable == "uploadpack" || variable == "receivepack")
	case "gpg":
		return variable == "program" || (sub == "ssh" && variable == "defaultkeycommand")
	}
	return false
}

// gitBool reports whether git reads value as a boolean rather than a command.
func gitBool(value string) bool {
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "1", "0":
		return true
	}
	return false
}
