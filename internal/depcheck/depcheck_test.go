package depcheck

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCompareOrdersVersionsBySemver2(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.0", 1},
		{"2.0.0", "10.0.0", -1},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3+build.5", "1.2.3+build.9", 0},
		// semver 2.0 §11: a pre-release sorts before its release.
		{"1.0.0-alpha", "1.0.0", -1},
		// The spec's own chain: 1.0.0-alpha < 1.0.0-alpha.1 < 1.0.0-alpha.beta <
		// 1.0.0-beta < 1.0.0-beta.2 < 1.0.0-beta.11 < 1.0.0-rc.1 < 1.0.0.
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.1+build", "1.0.0-rc.1", 0},
		{"1.0.0-99999999999999999999", "1.0.0-100000000000000000000", -1},
	}
	for _, c := range cases {
		a, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.a, err)
		}
		b, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.b, err)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(b, a); got != -c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestParseVersionRejectsWhatIsNotSemver(t *testing.T) {
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "1.2.x", "1.2.3-", "1.2.3-a..b", "1.2.-3", "link:../pkg"} {
		if v, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", s, v)
		}
	}
}

func TestVersionStringRoundTrips(t *testing.T) {
	for _, s := range []string{"1.2.3", "0.0.1-rc.1", "1.0.0-alpha.beta+exp.sha.5114f85"} {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatal(err)
		}
		if v.String() != s {
			t.Errorf("String() = %q, want %q", v.String(), s)
		}
	}
}

func TestRangeContainsWhatGitHubsRangeSyntaxNames(t *testing.T) {
	cases := []struct {
		rng string
		in  []string
		out []string
	}{
		{">= 1.0.0, < 1.2.3", []string{"1.0.0", "1.2.2", "1.2.3-rc.1"}, []string{"0.9.9", "1.2.3", "2.0.0", "1.0.0-rc.1"}},
		{"<= 2.0.1", []string{"0.0.1", "2.0.1", "2.0.1-beta"}, []string{"2.0.2", "3.0.0"}},
		{"< 4.17.21", []string{"4.17.20", "1.0.0"}, []string{"4.17.21", "4.18.0"}},
		{"> 1.0.0, <= 1.1.0", []string{"1.0.1", "1.1.0"}, []string{"1.0.0", "1.1.1"}},
		{"= 7.5.1", []string{"7.5.1"}, []string{"7.5.0", "7.5.2"}},
		{"7.5.1", []string{"7.5.1"}, []string{"7.5.2"}},
		{">=1.0.0,<2.0.0", []string{"1.5.0"}, []string{"2.0.0"}},
		// GitHub writes a short version on some advisories.
		{">= 4.0, < 4.1", []string{"4.0.0", "4.0.9"}, []string{"3.9.9", "4.1.0"}},
		{"< 2", []string{"1.9.9"}, []string{"2.0.0"}},
	}
	for _, c := range cases {
		r, err := ParseRange(c.rng)
		if err != nil {
			t.Fatalf("ParseRange(%q): %v", c.rng, err)
		}
		for _, s := range c.in {
			if !r.Contains(mustVersion(t, s)) {
				t.Errorf("%q should contain %s", c.rng, s)
			}
		}
		for _, s := range c.out {
			if r.Contains(mustVersion(t, s)) {
				t.Errorf("%q should not contain %s", c.rng, s)
			}
		}
	}
}

func TestParseRangeRejectsMalformedRanges(t *testing.T) {
	for _, s := range []string{"", " ", ">= 1.0.0,", ", < 2.0.0", "=> 1.0.0", "~1.2.3", "^1.2.3", ">= 1.0.0 < 2.0.0", "< x", "1.0.0 || 2.0.0"} {
		if r, err := ParseRange(s); err == nil {
			t.Errorf("ParseRange(%q) = %v, want an error", s, r)
		}
	}
}

func TestResolvedReadsPackagesFromAV6Lockfile(t *testing.T) {
	lock := readFixture(t, "pnpm-lock-v6.yaml")
	cases := []struct {
		pkg  string
		want []string
	}{
		{"semver", []string{"6.3.0", "7.5.1"}},
		{"@babel/core", []string{"7.22.5"}},
		{"react-dom", []string{"18.2.0"}},
		{"react", []string{"18.2.0"}},
		{"lodash", nil},
		// A prefix of another name is not that name.
		{"react-do", nil},
	}
	for _, c := range cases {
		assertResolved(t, lock, c.pkg, c.want)
	}
}

func TestResolvedReadsPackagesAndSnapshotsFromAV9Lockfile(t *testing.T) {
	lock := readFixture(t, "pnpm-lock-v9.yaml")
	cases := []struct {
		pkg  string
		want []string
	}{
		{"semver", []string{"6.3.1", "7.6.0"}},
		{"@babel/core", []string{"7.24.0"}},
		{"react-dom", []string{"18.3.1"}},
		{"js-tokens", []string{"4.0.0"}},
		{"lodash", nil},
	}
	for _, c := range cases {
		assertResolved(t, lock, c.pkg, c.want)
	}
}

// A snapshots: key is read on its own, peer suffix and all; an importers: entry
// (a specifier, not a resolution) is not.
func TestResolvedReadsASnapshotKeyAndIgnoresImporters(t *testing.T) {
	lock := []byte(strings.Join([]string{
		"lockfileVersion: '9.0'",
		"",
		"importers:",
		"",
		"  .:",
		"    dependencies:",
		"      '@scope/pkg':",
		"        specifier: ^1.0.0",
		"        version: 1.9.9",
		"",
		"snapshots:",
		"",
		"  \"@scope/pkg@1.2.3(peer@4.5.6)(@other/peer@1.0.0(deep@2.0.0))\":",
		"    dependencies:",
		"      peer: 4.5.6",
		"",
	}, "\r\n"))
	assertResolved(t, lock, "@scope/pkg", []string{"1.2.3"})
	assertResolved(t, lock, "peer", nil)
}

func TestResolvedRefusesALockfileItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		lock string
	}{
		{"no lockfileVersion", "packages:\n\n  /semver@7.5.1:\n    dev: false\n"},
		{"a v5 lockfile", "lockfileVersion: 5.4\n\npackages:\n\n  /semver/7.5.1:\n    dev: false\n"},
	}
	for _, c := range cases {
		if got, err := Resolved([]byte(c.lock), "semver"); err == nil {
			t.Errorf("%s: Resolved = %v, want an error", c.name, got)
		}
	}
}

// A resolution of the package that is not a registry version (a tarball, a git
// commit, a link) cannot be placed inside or outside a range, so the scan fails
// closed rather than report the package clean.
func TestResolvedFailsClosedOnAVersionItCannotParse(t *testing.T) {
	lock := []byte("lockfileVersion: '9.0'\n\npackages:\n\n  semver@https://codeload.github.com/npm/node-semver/tar.gz/abc123:\n    resolution: {tarball: x}\n\n  other@file:../other:\n    resolution: {directory: ../other}\n")
	if got, err := Resolved(lock, "semver"); err == nil {
		t.Errorf("Resolved = %v, want an error", got)
	}
	// Another package's unreadable resolution does not affect this one.
	assertResolved(t, lock, "lodash", nil)
}

// A v6 lockfile keys a git or tarball resolution by its source and names the
// package only in a `name:` field; that too fails closed rather than read clean.
func TestResolvedFailsClosedOnAV6GitResolution(t *testing.T) {
	lock := []byte("lockfileVersion: '6.0'\n\npackages:\n\n  github.com/npm/node-semver/abc123:\n    resolution: {tarball: https://codeload.github.com/npm/node-semver/tar.gz/abc123}\n    name: semver\n    version: 7.5.1\n    dev: false\n\n  /react@18.2.0:\n    name: react\n    dev: false\n")
	if got, err := Resolved(lock, "semver"); err == nil {
		t.Errorf("Resolved = %v, want an error", got)
	}
	// A registry entry that also carries `name:` is read as usual.
	assertResolved(t, lock, "react", []string{"18.2.0"})
}

func TestResolvedRefusesAnEmptyPackageName(t *testing.T) {
	if got, err := Resolved(readFixture(t, "pnpm-lock-v9.yaml"), ""); err == nil {
		t.Errorf("Resolved = %v, want an error", got)
	}
}

func TestVulnerableNamesTheResolvedVersionsInsideAnyRange(t *testing.T) {
	ranges := []Range{mustRange(t, "< 6.3.1"), mustRange(t, ">= 7.0.0, < 7.5.2")}
	cases := []struct {
		fixture string
		want    []string
	}{
		{"pnpm-lock-v6.yaml", []string{"6.3.0", "7.5.1"}},
		{"pnpm-lock-v9.yaml", nil},
	}
	for _, c := range cases {
		got, err := Vulnerable(readFixture(t, c.fixture), "semver", ranges)
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		if !reflect.DeepEqual(strs(got), c.want) {
			t.Errorf("%s: Vulnerable = %v, want %v", c.fixture, strs(got), c.want)
		}
	}
}

func assertResolved(t *testing.T, lock []byte, pkg string, want []string) {
	t.Helper()
	got, err := Resolved(lock, pkg)
	if err != nil {
		t.Fatalf("Resolved(%q): %v", pkg, err)
	}
	if !reflect.DeepEqual(strs(got), want) {
		t.Errorf("Resolved(%q) = %v, want %v", pkg, strs(got), want)
	}
}

func strs(vs []Version) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.String())
	}
	return out
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

func mustRange(t *testing.T, s string) Range {
	t.Helper()
	r, err := ParseRange(s)
	if err != nil {
		t.Fatalf("ParseRange(%q): %v", s, err)
	}
	return r
}
