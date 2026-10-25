package verify

import "testing"

// truth is a scripted [GroundTruth]: the host-side git reads the decisions in this
// package make for themselves, as named fields. It keeps the truth tables below
// flat and fast while the reads themselves are exercised against real git one layer
// down — Worktree.CommitsAhead, DisjointHistory, DiffEmpty and the rest have
// temp-repo tests in internal/git — and the whole composition runs over a real temp
// repo in internal/stages.
//
// reads records every read in order, so a test can assert not just the verdict but
// which ground truth the decision was willing to spend to reach it.
type truth struct {
	t *testing.T

	exists    bool
	clean     bool
	ahead     int
	disjoint  bool
	diffEmpty bool
	branch    bool
	pushed    bool
	rebased   bool
	pr        bool

	reads []string
}

// The port is the whole contract: every decision must be reachable with nothing but
// these reads, so a change that reintroduces a caller-filled parameter struct has to
// come through this assertion first.
var _ GroundTruth = (*truth)(nil)

func (g *truth) read(name string) { g.reads = append(g.reads, name) }

func (g *truth) Exists() bool          { g.read("Exists"); return g.exists }
func (g *truth) Clean() bool           { g.read("Clean"); return g.clean }
func (g *truth) CommitsAhead() int     { g.read("CommitsAhead"); return g.ahead }
func (g *truth) DisjointHistory() bool { g.read("DisjointHistory"); return g.disjoint }
func (g *truth) DiffEmpty() bool       { g.read("DiffEmpty"); return g.diffEmpty }
func (g *truth) BranchExists() bool    { g.read("BranchExists"); return g.branch }
func (g *truth) BranchPushed() bool    { g.read("BranchPushed"); return g.pushed }
func (g *truth) PRExists() bool        { g.read("PRExists"); return g.pr }

func (g *truth) IsRebasedOnto(ref string) bool {
	g.t.Helper()
	if ref != baseRef {
		g.t.Errorf("IsRebasedOnto(%q), want the decision to measure against %s", ref, baseRef)
	}
	g.read("IsRebasedOnto")
	return g.rebased
}
