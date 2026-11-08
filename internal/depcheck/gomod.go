package depcheck

import (
	"fmt"
	"strconv"
	"strings"
)

// ResolvedGoMod returns every version of module that a go.mod puts in the build,
// in ascending order with no repeats. Since Go 1.17 a module's go.mod lists every
// module its build loads, at the version minimal version selection picked, so the
// require lines are the resolution, as a pnpm lockfile's keys are. A replace
// directive changes what is loaded, so it wins over the require it matches.
//
// It fails closed. A go.mod with no go directive, or one older than 1.17 (whose
// go.mod may omit indirect modules), is an error, as is a replacement of module
// by a local directory or by a different module, which no range can place.
func ResolvedGoMod(gomod []byte, module string) ([]Version, error) {
	if module == "" {
		return nil, fmt.Errorf("go.mod: no module path")
	}
	f, err := parseGoMod(gomod)
	if err != nil {
		return nil, err
	}
	var found []Version
	add := func(s string) error {
		v, err := ParseVersion(s)
		if err != nil {
			return fmt.Errorf("go.mod: %s resolves to %q, which is not a semver version: %w", module, s, err)
		}
		found = append(found, v)
		return nil
	}
	for _, req := range f.requires {
		if req.path != module {
			continue
		}
		rep, ok := f.replacementOf(req.path, req.version)
		if !ok {
			if err := add(req.version); err != nil {
				return nil, err
			}
			continue
		}
		switch {
		case rep.newVersion == "":
			return nil, fmt.Errorf("go.mod: %s is replaced by the local directory %s, which no range can place", module, rep.newPath)
		case rep.newPath != module:
			return nil, fmt.Errorf("go.mod: %s is replaced by another module, %s %s, which its alerts do not cover", module, rep.newPath, rep.newVersion)
		}
		if err := add(rep.newVersion); err != nil {
			return nil, err
		}
	}
	// Another module replaced by this one loads this one's code too.
	for _, rep := range f.replaces {
		if rep.newPath == module && rep.oldPath != module && rep.newVersion != "" {
			if err := add(rep.newVersion); err != nil {
				return nil, err
			}
		}
	}
	return uniqueSorted(found), nil
}

// goMod is the part of a go.mod the reader needs.
type goMod struct {
	requires []goRequire
	replaces []goReplace
}

type goRequire struct{ path, version string }

// goReplace is `old [oldVersion] => new [newVersion]`; a newVersion of "" is a
// local directory.
type goReplace struct{ oldPath, oldVersion, newPath, newVersion string }

// replacementOf is the replace directive that applies to path at version: one
// naming that exact version wins over one naming none, as in the go command.
func (f goMod) replacementOf(path, version string) (goReplace, bool) {
	var any *goReplace
	for i, r := range f.replaces {
		if r.oldPath != path {
			continue
		}
		if r.oldVersion == version {
			return r, true
		}
		if r.oldVersion == "" {
			any = &f.replaces[i]
		}
	}
	if any != nil {
		return *any, true
	}
	return goReplace{}, false
}

// parseGoMod reads the go, require and replace directives, single-line or in a
// block, line by line.
func parseGoMod(data []byte) (goMod, error) {
	var f goMod
	goVersion, block := "", ""
	for n, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := raw
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var directive string
		var args []string
		switch {
		case block != "" && fields[0] == ")":
			block = ""
			continue
		case block != "":
			directive, args = block, fields
		case fields[0] == "go" && len(fields) == 2:
			goVersion = fields[1]
			continue
		case (fields[0] == "require" || fields[0] == "replace") && len(fields) == 2 && fields[1] == "(":
			block = fields[0]
			continue
		case fields[0] == "require" || fields[0] == "replace":
			directive, args = fields[0], fields[1:]
		default:
			continue
		}
		for i := range args {
			if p, err := strconv.Unquote(args[i]); err == nil {
				args[i] = p
			}
		}
		switch directive {
		case "require":
			if len(args) != 2 {
				return goMod{}, fmt.Errorf("go.mod:%d: malformed require %q", n+1, strings.TrimSpace(raw))
			}
			f.requires = append(f.requires, goRequire{args[0], args[1]})
		case "replace":
			r, err := parseReplace(args)
			if err != nil {
				return goMod{}, fmt.Errorf("go.mod:%d: %w", n+1, err)
			}
			f.replaces = append(f.replaces, r)
		}
	}
	if goVersion == "" {
		return goMod{}, fmt.Errorf("go.mod has no go directive, so it may not list every module the build loads")
	}
	if !atLeastGo117(goVersion) {
		return goMod{}, fmt.Errorf("go.mod says go %s: before 1.17 it need not list every module the build loads", goVersion)
	}
	return f, nil
}

// parseReplace reads `old [v] => new [v]`.
func parseReplace(args []string) (goReplace, error) {
	arrow := -1
	for i, a := range args {
		if a == "=>" {
			arrow = i
		}
	}
	left, right := args, []string(nil)
	if arrow >= 0 {
		left, right = args[:arrow], args[arrow+1:]
	}
	if arrow < 0 || len(left) < 1 || len(left) > 2 || len(right) < 1 || len(right) > 2 {
		return goReplace{}, fmt.Errorf("malformed replace %q", strings.Join(args, " "))
	}
	r := goReplace{oldPath: left[0], newPath: right[0]}
	if len(left) == 2 {
		r.oldVersion = left[1]
	}
	if len(right) == 2 {
		r.newVersion = right[1]
	}
	return r, nil
}

// atLeastGo117 reports whether a go directive's version is 1.17 or later.
func atLeastGo117(v string) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err1 != nil || err2 != nil {
		return false
	}
	return major > 1 || major == 1 && minor >= 17
}
