package verify

import "testing"

// slug is the one ticket every test in this package decides about. The decisions
// thread it into each ground-truth read unchanged, and [truth] asserts that, so a
// decision that read the wrong ticket's worktree fails here rather than shipping.
const slug = "beh-1"

// truth is a scripted [GroundTruth]: the host-side git reads the decisions in this
// package now make for themselves, as named fields. It keeps the truth tables below
// flat and fast while the reads themselves are exercised against real git one layer
// down — Worktree.CommitsAhead, DisjointHistory, DiffEmpty and the rest have
// temp-repo tests in internal/git — and the whole composition runs through
// hostio.Fake in internal/stages. hostio.Host cannot stand in here: it is the
// adapter that satisfies this port, so importing it would close a cycle.
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

func (g *truth) read(name, gotSlug string) {
	g.t.Helper()
	if gotSlug != slug {
		g.t.Errorf("%s read ground truth for slug %q, want %q", name, gotSlug, slug)
	}
	g.reads = append(g.reads, name)
}

func (g *truth) WorktreeExists(s string) bool  { g.read("WorktreeExists", s); return g.exists }
func (g *truth) WorktreeClean(s string) bool   { g.read("WorktreeClean", s); return g.clean }
func (g *truth) CommitsAhead(s string) int     { g.read("CommitsAhead", s); return g.ahead }
func (g *truth) BranchDisjoint(s string) bool  { g.read("BranchDisjoint", s); return g.disjoint }
func (g *truth) BranchDiffEmpty(s string) bool { g.read("BranchDiffEmpty", s); return g.diffEmpty }
func (g *truth) BranchExists(s string) bool    { g.read("BranchExists", s); return g.branch }
func (g *truth) BranchPushed(s string) bool    { g.read("BranchPushed", s); return g.pushed }
func (g *truth) IsRebased(s string) bool       { g.read("IsRebased", s); return g.rebased }
func (g *truth) PRExists(s string) bool        { g.read("PRExists", s); return g.pr }
