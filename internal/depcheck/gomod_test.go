package depcheck

import (
	"reflect"
	"strings"
	"testing"
)

const goModFixture = `module example.com/app

go 1.22

toolchain go1.22.4

require (
	github.com/pkg/errors v0.9.1
	golang.org/x/net v0.17.0 // indirect
	"golang.org/x/text" v0.13.0
)

require golang.org/x/crypto v0.14.0

require github.com/old/thing v2.0.0+incompatible // indirect
require golang.org/x/sys v0.0.0-20231010123456-abcdef123456 // indirect
`

// The Go reader answers from go.mod: since Go 1.17 its require lines name every
// module the build loads, at the version minimal version selection picked. A
// single-line or block require, a quoted path, an // indirect comment, a
// pseudo-version and +incompatible are all read.
func TestResolvedGoModReadsEveryRequireForm(t *testing.T) {
	for _, c := range []struct {
		module string
		want   []string
	}{
		{"golang.org/x/net", []string{"0.17.0"}},
		{"golang.org/x/crypto", []string{"0.14.0"}},
		{"golang.org/x/text", []string{"0.13.0"}},
		{"github.com/pkg/errors", []string{"0.9.1"}},
		{"github.com/old/thing", []string{"2.0.0+incompatible"}},
		{"golang.org/x/sys", []string{"0.0.0-20231010123456-abcdef123456"}},
		{"golang.org/x/absent", nil},
	} {
		got, err := ResolvedGoMod([]byte(goModFixture), c.module)
		if err != nil {
			t.Fatalf("%s: %v", c.module, err)
		}
		var s []string
		for _, v := range got {
			s = append(s, v.String())
		}
		if !reflect.DeepEqual(s, c.want) {
			t.Errorf("%s resolves %v, want %v", c.module, s, c.want)
		}
	}
}

// A module path is matched whole: golang.org/x/net is not golang.org/x/net/v2,
// and a module named only as the prefix of another is not found.
func TestResolvedGoModMatchesTheWholeModulePath(t *testing.T) {
	mod := "module m\n\ngo 1.21\n\nrequire (\n\tgithub.com/a/b/v2 v2.1.0\n\tgithub.com/a/bc v1.0.0\n)\n"
	for _, module := range []string{"github.com/a/b", "github.com/a"} {
		got, err := ResolvedGoMod([]byte(mod), module)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v; want nothing", module, got, err)
		}
	}
}

// A replace directive changes what the build loads, so it wins over require: a
// versioned replacement by the same module is that version; a replacement of
// another module by this one adds its version; and a replacement by a local
// directory or a different module cannot be placed in a range, so it fails closed.
func TestResolvedGoModFollowsReplaceDirectives(t *testing.T) {
	base := "module m\n\ngo 1.21\n\nrequire golang.org/x/net v0.17.0\n"
	for _, c := range []struct {
		name, replace string
		want          []string
		wantErr       string
	}{
		{"same module, new version", "replace golang.org/x/net => golang.org/x/net v0.23.0\n", []string{"0.23.0"}, ""},
		{"only the vulnerable version", "replace golang.org/x/net v0.17.0 => golang.org/x/net v0.23.0\n", []string{"0.23.0"}, ""},
		{"another version's replace", "replace golang.org/x/net v0.10.0 => golang.org/x/net v0.23.0\n", []string{"0.17.0"}, ""},
		{"block form", "replace (\n\tgolang.org/x/net => golang.org/x/net v0.23.0\n)\n", []string{"0.23.0"}, ""},
		{"local directory", "replace golang.org/x/net => ../net\n", nil, "local directory"},
		{"a fork", "replace golang.org/x/net => github.com/fork/net v0.23.0\n", nil, "another module"},
		{"replaced in by another", "replace example.com/other => golang.org/x/net v0.9.0\n", []string{"0.9.0", "0.17.0"}, ""},
	} {
		got, err := ResolvedGoMod([]byte(base+c.replace), "golang.org/x/net")
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var s []string
		for _, v := range got {
			s = append(s, v.String())
		}
		if !reflect.DeepEqual(s, c.want) {
			t.Errorf("%s: resolves %v, want %v", c.name, s, c.want)
		}
	}
}

// Before Go 1.17 go.mod need not list the modules the build loads indirectly, so
// a module missing from it may still be in the build: such a go.mod, or one with
// no go directive at all, fails closed rather than reading as clean.
func TestResolvedGoModFailsClosedWhenGoModIsNotComplete(t *testing.T) {
	for _, mod := range []string{
		"module m\n\ngo 1.16\n\nrequire golang.org/x/net v0.17.0\n",
		"module m\n\nrequire golang.org/x/net v0.17.0\n",
		"",
	} {
		if got, err := ResolvedGoMod([]byte(mod), "golang.org/x/net"); err == nil {
			t.Errorf("ResolvedGoMod(%q) = %v, want an error", mod, got)
		}
	}
	if _, err := ResolvedGoMod([]byte("module m\n\ngo 1.21\n\nrequire golang.org/x/net v0.17.0\n"), ""); err == nil {
		t.Error("an empty module path is an error")
	}
}

// The Go ecosystem checks only the repository's root module: each nested go.mod
// is its own module, which a scan of the root go.mod says nothing about. The
// files its fix may change are go.mod, go.sum and the vendor tree.
func TestTheGoEcosystem(t *testing.T) {
	eco, ok := ForEcosystem("go")
	if !ok {
		t.Fatal("no Go ecosystem")
	}
	if eco.Lockfile != "go.mod" {
		t.Errorf("Lockfile = %q", eco.Lockfile)
	}
	for path, want := range map[string]bool{"go.mod": true, "tools/go.mod": false, "go.sum": false, "package.json": false} {
		if got := eco.ChecksManifest(path); got != want {
			t.Errorf("ChecksManifest(%q) = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string]bool{"go.mod": true, "go.sum": true, "vendor/modules.txt": true, "vendor/golang.org/x/net/http2/frame.go": true, "tools/go.mod": false, "main.go": false, "internal/vendor/x.go": false} {
		if got := eco.IsDependencyFile(path); got != want {
			t.Errorf("IsDependencyFile(%q) = %v, want %v", path, got, want)
		}
	}
	r := eco.Check([]byte("module m\n\ngo 1.21\n\nrequire golang.org/x/net v0.17.0\n"), "golang.org/x/net", []string{"< 0.23.0"})
	if r.Err != nil || len(r.Vulnerable) != 1 || r.Vulnerable[0].String() != "0.17.0" {
		t.Errorf("Check = %+v, want 0.17.0 vulnerable", r)
	}
}

// The npm ecosystem is the pnpm reader as it was: the root pnpm-lock.yaml, alerts
// in a package.json at any depth, and the three pnpm files.
func TestTheNPMEcosystem(t *testing.T) {
	eco, ok := ForEcosystem("npm")
	if !ok {
		t.Fatal("no npm ecosystem")
	}
	if eco.Lockfile != "pnpm-lock.yaml" {
		t.Errorf("Lockfile = %q", eco.Lockfile)
	}
	for path, want := range map[string]bool{"package.json": true, "apps/web/package.json": true, "pnpm-lock.yaml": true, "yarn.lock": false} {
		if got := eco.ChecksManifest(path); got != want {
			t.Errorf("ChecksManifest(%q) = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string]bool{"package.json": true, "apps/web/package.json": true, "pnpm-lock.yaml": true, "pnpm-workspace.yaml": true, "src/index.ts": false} {
		if got := eco.IsDependencyFile(path); got != want {
			t.Errorf("IsDependencyFile(%q) = %v, want %v", path, got, want)
		}
	}
	if _, ok := ForEcosystem("pip"); ok {
		t.Error("pip has no lockfile reader")
	}
}
