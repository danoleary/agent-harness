package overlap

import (
	"reflect"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// The herd sibling tickets (#97) named their files three ways: a full path, a bare
// file name in a table, and a module name in backticks. Each one predicts the
// in-flight files it names, and nothing else.
func TestMatchPredictsTheFilesABodyNames(t *testing.T) {
	inFlight := []string{
		"web/src/modules/space/host-document-session.ts",
		"web/src/modules/space/document-page-controls.tsx",
		"web/src/modules/space/document-page-controls.test.tsx",
		"web/src/modules/call/call-room.ts",
		"internal/loop/loop.go",
		"README.md",
	}
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a full path",
			body: "Slow the page turn in web/src/modules/space/host-document-session.ts.",
			want: []string{"web/src/modules/space/host-document-session.ts"},
		},
		{
			name: "a path with a line number, in backticks",
			body: "See `web/src/modules/space/host-document-session.ts:42`.",
			want: []string{"web/src/modules/space/host-document-session.ts"},
		},
		{
			name: "a directory names every file under it",
			body: "Everything lives under `web/src/modules/space/`.",
			want: []string{
				"web/src/modules/space/document-page-controls.test.tsx",
				"web/src/modules/space/document-page-controls.tsx",
				"web/src/modules/space/host-document-session.ts",
			},
		},
		{
			name: "a tail of a path",
			body: "The fix belongs in call/call-room.ts.",
			want: []string{"web/src/modules/call/call-room.ts"},
		},
		{
			name: "a bare file name names its tests and stories too",
			body: "| 14 | document-page-controls.tsx |",
			want: []string{
				"web/src/modules/space/document-page-controls.test.tsx",
				"web/src/modules/space/document-page-controls.tsx",
			},
		},
		{
			name: "a module name in backticks",
			body: "Teach `host-document-session` the backdrop.",
			want: []string{"web/src/modules/space/host-document-session.ts"},
		},
		{
			name: "a package name in backticks names its directory's files",
			body: "The daemon in `loop` owns it.",
			want: []string{"internal/loop/loop.go"},
		},
		{
			name: "a bare word in prose names nothing",
			body: "The loop should not loop forever over the space.",
		},
		{
			name: "a URL is not a path",
			body: "See https://github.com/o/r/blob/main/internal/loop/loop.go and github.com/o/r/pull/3.",
		},
		{
			name: "a file that is not in flight",
			body: "Change internal/stages/review.go.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Match(Predict(c.body), inFlight)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func prWork(label, branch string, files ...string) Work {
	return Work{Label: label, Branch: branch, Files: files}
}

func TestCheckDefersOnTheFirstWorkPastTheThreshold(t *testing.T) {
	tk := ticket.Ticket{
		Identifier:  "#97",
		Title:       "Speed up the page turn",
		Description: "Edit `host-document-session` and web/src/modules/space/document-page-controls.tsx.",
	}
	work := []Work{
		prWork("PR #10", "feat/gh-1", "internal/loop/loop.go"),
		prWork("PR #11", "feat/gh-2", "web/src/modules/space/host-document-session.ts", "web/src/modules/space/document-page-controls.tsx"),
		prWork("PR #12", "feat/gh-3", "web/src/modules/space/host-document-session.ts"),
	}

	cases := []struct {
		name      string
		policy    Policy
		ticket    ticket.Ticket
		wantLabel string
		wantFiles []string
	}{
		{
			name:      "one shared file defers by default",
			policy:    Policy{Threshold: 1},
			ticket:    tk,
			wantLabel: "PR #11",
			wantFiles: []string{"web/src/modules/space/document-page-controls.tsx", "web/src/modules/space/host-document-session.ts"},
		},
		{
			name:      "a higher threshold needs more shared files",
			policy:    Policy{Threshold: 2},
			ticket:    tk,
			wantLabel: "PR #11",
			wantFiles: []string{"web/src/modules/space/document-page-controls.tsx", "web/src/modules/space/host-document-session.ts"},
		},
		{
			name:   "a threshold above every overlap dispatches",
			policy: Policy{Threshold: 3},
			ticket: tk,
		},
		{
			name:   "threshold 0 turns the check off",
			policy: Policy{Threshold: 0},
			ticket: tk,
		},
		{
			name:   "the escape label dispatches in parallel",
			policy: Policy{Threshold: 1, EscapeLabel: "parallel-ok"},
			ticket: func() ticket.Ticket { t := tk; t.Labels = []string{"bug", "parallel-ok"}; return t }(),
		},
		{
			name:   "a ticket that names no in-flight file dispatches as today",
			policy: Policy{Threshold: 1},
			ticket: ticket.Ticket{Identifier: "#98", Description: "Fix internal/stages/review.go."},
		},
		{
			name:      "the title predicts files too",
			policy:    Policy{Threshold: 1},
			ticket:    ticket.Ticket{Identifier: "#99", Title: "Tidy `loop`"},
			wantLabel: "PR #10",
			wantFiles: []string{"internal/loop/loop.go"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, deferred := Check(c.ticket, "feat/gh-97", work, c.policy)
			if deferred != (c.wantLabel != "") {
				t.Fatalf("deferred = %v (%+v), want %v", deferred, d, c.wantLabel != "")
			}
			if !deferred {
				return
			}
			if d.Work.Label != c.wantLabel || !reflect.DeepEqual(d.Files, c.wantFiles) {
				t.Errorf("deferral = %s on %v, want %s on %v", d.Work.Label, d.Files, c.wantLabel, c.wantFiles)
			}
		})
	}
}

// The candidate's own branch is not someone else's work: a ticket re-dispatched
// over its own claimed branch never defers behind itself.
func TestCheckIgnoresTheTicketsOwnBranch(t *testing.T) {
	tk := ticket.Ticket{Identifier: "#97", Description: "Edit internal/loop/loop.go."}
	work := []Work{prWork("#97 (claimed)", "feat/gh-97", "internal/loop/loop.go")}

	if d, deferred := Check(tk, "feat/gh-97", work, Policy{Threshold: 1}); deferred {
		t.Errorf("deferred behind its own branch: %+v", d)
	}
}

func TestDeferralReason(t *testing.T) {
	d := Deferral{Work: prWork("PR #11", "feat/gh-2"), Files: []string{"a.go", "b.go"}}
	if got, want := d.Reason(), "deferred: overlaps PR #11 on a.go, b.go"; got != want {
		t.Errorf("Reason() = %q, want %q", got, want)
	}
}
