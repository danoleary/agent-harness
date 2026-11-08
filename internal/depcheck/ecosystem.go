package depcheck

import (
	"path"
	"strings"
)

// Ecosystem is what the host needs to check one Dependabot ecosystem's fix: the
// file it scans, which alerts that scan covers, which files a fix may change, and
// how to read a resolved version out of the scanned file (ADR-0040, decision 4).
type Ecosystem struct {
	// Name is Dependabot's name for the ecosystem ("npm", "go").
	Name string
	// Lockfile is the root-relative file Check reads.
	Lockfile string
	// manifests are the base names of the manifests whose alerts the scan covers;
	// rootOnly limits them to the repository root.
	manifests []string
	rootOnly  bool
	// files are the base names a fix may change; rootFiles holds them to the
	// root, and dirs are root-relative trees a fix may change anything in.
	files     []string
	rootFiles bool
	dirs      []string
	resolve   func(lock []byte, pkg string) ([]Version, error)
}

// ecosystems are the ecosystems with a reader. An alert on any other is skipped:
// its fix could not be checked.
var ecosystems = []Ecosystem{
	{
		Name: "npm", Lockfile: "pnpm-lock.yaml",
		// pnpm keeps one lockfile for the whole workspace, so a package.json at
		// any depth is covered by the root scan.
		manifests: []string{"pnpm-lock.yaml", "package.json"},
		files:     []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"},
		resolve:   Resolved,
	},
	{
		Name: "go", Lockfile: "go.mod",
		// Each go.mod is its own module with its own resolution, so a scan of the
		// root go.mod covers the root module's alerts and nothing nested.
		manifests: []string{"go.mod"}, rootOnly: true,
		files: []string{"go.mod", "go.sum"}, rootFiles: true,
		dirs:    []string{"vendor"},
		resolve: ResolvedGoMod,
	},
}

// ForEcosystem is the ecosystem Dependabot calls name, if the host can check it.
func ForEcosystem(name string) (Ecosystem, bool) {
	for _, e := range ecosystems {
		if e.Name == name {
			return e, true
		}
	}
	return Ecosystem{}, false
}

// ChecksManifest reports whether an alert in the manifest at the repo-relative
// path p is covered by the scan of Lockfile.
func (e Ecosystem) ChecksManifest(p string) bool {
	if e.rootOnly && path.Dir(p) != "." {
		return false
	}
	return contains(e.manifests, path.Base(p))
}

// IsDependencyFile reports whether a fix may change the repo-relative path p.
func (e Ecosystem) IsDependencyFile(p string) bool {
	for _, d := range e.dirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	if e.rootFiles && path.Dir(p) != "." {
		return false
	}
	return contains(e.files, path.Base(p))
}

// DependencyFiles names the files a fix may change, for a message or a prompt.
func (e Ecosystem) DependencyFiles() string {
	names := make([]string, 0, len(e.files)+len(e.dirs))
	for _, f := range e.files {
		names = append(names, "`"+f+"`")
	}
	for _, d := range e.dirs {
		names = append(names, "`"+d+"/`")
	}
	switch len(names) {
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// Check scans lock, the ecosystem's Lockfile, for pkg against ranges, written in
// GitHub's syntax as the alerts carry them.
func (e Ecosystem) Check(lock []byte, pkg string, ranges []string) Result {
	return check(e.resolve, lock, pkg, ranges)
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
