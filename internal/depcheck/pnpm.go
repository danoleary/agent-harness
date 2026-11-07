package depcheck

import (
	"fmt"
	"sort"
	"strings"
)

// Resolved returns every version of pkg that a pnpm v6 or v9 lockfile resolves,
// in ascending order with no repeats. It reads the keys under `packages:` and
// `snapshots:`: `/pkg@1.2.3` (v6), `pkg@1.2.3` (v9), a scoped or quoted key, and
// a peer suffix (`pkg@1.2.3(peer@4.5.6)`).
//
// It fails closed. A lockfile with no lockfileVersion, or a version it does not
// read, is an error; so is a resolution of pkg that is not a semver version (a
// tarball, a git commit, a link), which no range can place.
func Resolved(lock []byte, pkg string) ([]Version, error) {
	lines := strings.Split(strings.ReplaceAll(string(lock), "\r\n", "\n"), "\n")
	if err := checkLockfileVersion(lines); err != nil {
		return nil, err
	}
	var found []Version
	section := ""
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' {
			section = strings.TrimSpace(line)
			continue
		}
		if section != "packages:" && section != "snapshots:" {
			continue
		}
		// An entry is a key at an indent of two; deeper lines are its fields.
		if !strings.HasPrefix(line, "  ") || len(line) < 3 || line[2] == ' ' {
			continue
		}
		name, version := splitKey(entryKey(line[2:]))
		if name != pkg {
			continue
		}
		v, err := ParseVersion(version)
		if err != nil {
			return nil, fmt.Errorf("pnpm lockfile: %s resolves to %q, which is not a semver version: %w", pkg, version, err)
		}
		found = append(found, v)
	}
	return uniqueSorted(found), nil
}

// checkLockfileVersion accepts the lockfile formats whose keys Resolved reads:
// v6 (pnpm 8) and v9 (pnpm 9 and later).
func checkLockfileVersion(lines []string) error {
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, "lockfileVersion:")
		if !ok {
			continue
		}
		v := strings.Trim(strings.TrimSpace(rest), `'"`)
		major, _, _ := strings.Cut(v, ".")
		if major != "6" && major != "9" {
			return fmt.Errorf("pnpm lockfile: lockfileVersion %s is not supported (want 6 or 9)", v)
		}
		return nil
	}
	return fmt.Errorf("pnpm lockfile: no lockfileVersion")
}

// entryKey returns the mapping key of an entry line: the text inside its quotes,
// or the text before its `:` (`pkg@1.2.3:`, `pkg@1.2.3: {}`).
func entryKey(s string) string {
	if q := s[0]; q == '\'' || q == '"' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : end+1]
		}
	}
	if k, _, ok := strings.Cut(s, ": "); ok {
		return k
	}
	return strings.TrimSuffix(strings.TrimSpace(s), ":")
}

// splitKey splits a key into package name and version: `/@scope/pkg@1.2.3(peer@4.5.6)`
// is `@scope/pkg` and `1.2.3`. The name ends at the first `@` past a scope's.
func splitKey(key string) (name, version string) {
	key = strings.TrimPrefix(key, "/")
	at := strings.IndexByte(strings.TrimPrefix(key, "@"), '@')
	if at < 0 {
		return "", ""
	}
	if strings.HasPrefix(key, "@") {
		at++
	}
	name, version = key[:at], key[at+1:]
	if i := strings.IndexByte(version, '('); i >= 0 {
		version = version[:i]
	}
	return name, version
}

func uniqueSorted(vs []Version) []Version {
	sort.Slice(vs, func(i, j int) bool { return Compare(vs[i], vs[j]) < 0 })
	var out []Version
	for _, v := range vs {
		if len(out) > 0 && Compare(out[len(out)-1], v) == 0 {
			continue
		}
		out = append(out, v)
	}
	return out
}
