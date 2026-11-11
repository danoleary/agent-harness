// Package prompt builds the `-p` prompts for the sandboxed Claude sessions the
// harness drives (implementation, review, retrospective).
//
// Each Stage prompt is composed as a non-overridable **contract envelope** around
// a Consumer-supplied **body** (ADR-0009). The body — which skill to invoke,
// project conventions, gate hints — is read from the bind-mounted checkout's
// `.agent-harness/prompts/{implement,review,retro}.md` and passed in by the
// caller. The envelope is harness-owned Go: it injects the ticket context, the
// findings-dropbox protocol, the tracker-off steer, the branch/handoff contract,
// and the sandbox bash-quirk steer. Because the envelope is appended structurally
// (not merged into the body), a body that omits or contradicts a contract item
// cannot drop it — the guarantee is the harness's public API.
package prompt

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/sentry"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// bashQuirkSteer warns the sandboxed agent off four opaque-error surfaces in the
// pinned Claude CLI's bash wrapper that read like the agent's own bug but are an
// environment artifact. It is envelope content: every stage prompt carries it,
// regardless of the Consumer body. (BEH-401) Chaining a pipe into `head`/`tail`
// with a command substitution like `cd "$(...)"` in a single Bash call gets
// misparsed into errors like `head: invalid number of bytes` / `cd: too many
// arguments`. (BEH-598) When a plain command exits non-zero BY DESIGN, the tool
// can collapse that into a bare `Error` string with the real exit code and stderr
// stripped. (BEH-601) A `VAR=value; cmd "$VAR"` assignment-then-use within ONE
// Bash call can expand $VAR to the empty string. (BEH-645) Chaining trailing
// statements onto a GATE command in one Bash call can concatenate them as
// ARGUMENTS onto the gate's own command — a spurious RED on a gate that actually
// PASSED. (#290) Because `$?` does not survive between Bash calls, the steer
// tells the agent to read a lone gate call's exit status off the tool result
// itself — no `Error` marker means exit 0 — rather than re-running the gate.
//
// The quirks are properties of the CLI and of POSIX shell, so the steer describes
// them in those terms only. It named herd's pnpm/oxfmt gate output verbatim until
// BEH-641, which made the envelope — the harness's own public API (ADR-0009) —
// carry one Consumer's toolchain to every other Consumer's agent.
const bashQuirkSteer = "Sandbox bash quirk (BEH-401/BEH-598/BEH-601/BEH-645): the bundled Claude CLI's Bash tool can surface opaque errors that look like a bug in your command but are an environment artifact, in four ways. (1) It intermittently mangles a single Bash call that BOTH pipes into `head`/`tail` AND uses a command substitution like `cd \"$(...)\"`, producing errors like `head: invalid number of bytes: 'set -euo pipefail; ...'` or `cd: too many arguments`. (2) When a plain command exits non-zero BY DESIGN, the tool can collapse that into a bare `Error` string with the real exit code and stderr stripped — e.g. `git merge-base HEAD origin/main` exits 1 when two commits share no common ancestor, which is an expected signal, not a failure. (3) A `VAR=value; cmd \"$VAR\"` assignment-then-use within ONE Bash call can expand `$VAR` to the EMPTY string: the failure is silent (empty output) or surfaces as a path with the prefix missing (e.g. `\"$DP/dist\"` becomes `/dist` → `cannot access '/dist'`). `&&`-chaining the assignment to its use (`VAR=value && cmd \"$VAR\"`) expands fine; `;`-separating it is what drops the variable. (4) THE WORST, because it makes a GREEN gate look RED: chaining trailing statements onto a GATE command in one Bash call — e.g. `<gate command> > /tmp/gate.log 2>&1; echo exit=$?; grep -i error /tmp/gate.log | head` — can concatenate those trailing statements as ARGUMENTS onto the gate's own command, so the underlying tool receives `echo exit=$? grep …` where it expected files or targets. It then fails with an argument/target complaint (a formatter reporting it matched no input files, a test runner reporting an unknown target) and your task runner reports a non-zero lifecycle exit — a spurious failure on a gate that actually PASSED. Work around all four: run one command per Bash call, prefer absolute paths over `cd \"$(...)\"`, and don't tack `| head -n N` onto a compound command; when a plain command (not a gate — see below) returns a bare `Error`, do NOT assume it broke — re-run it capturing the exit code explicitly (append `; echo exit=$?`, or use `cmd || echo \"exit $?\"`) to tell an expected non-zero exit from a real failure; avoid intra-call shell variables — inline the absolute path, `&&`-chain instead of `;`, split into separate calls, or use the Grep/Glob tools with literal absolute paths; and run each configured verification gate as its OWN Bash call with NOTHING appended — no `; echo exit=$?`, no `> log 2>&1; grep … | head` — and take the tool's answer AS its exit status, because `$?` does not carry over to a later Bash call: a gate call that returns without the tool's `Error` marker (a bare `Error` or `Exit code N`) exited 0, a bare `(Bash completed with no output)` included, while a non-zero exit always comes back marked `Error` as in (2). So do NOT re-run a gate just to learn its exit status: on green move on, on red read the output it returned for the failure line; and if a gate reds with an argument/target complaint naming input it was never meant to receive, re-run it ALONE before treating it as a real failure. Retrying verbatim won't help in any case — split it up, inspect the exit code, drop the intra-call variable, or re-run the gate alone."

// Stage names which sandboxed session a prompt drives. It is the one axis the
// caller chooses; everything that varies within a Stage (resume state, filed
// findings, CI logs) is a field of Context, so a new variant costs no interface.
type Stage int

const (
	// Implement is the implementation session; Context.Resume picks how it enters
	// its worktree.
	Implement Stage = iota
	// Review is the cold review session over the handed-off branch.
	Review
	// Retrospective is the terminal findings-only session.
	Retrospective
	// CIFix diagnoses and fixes a red CI check on the open PR (BEH-414).
	CIFix
	// RebaseFix resolves a genuine conflict in the pre-push rebase (BEH-581).
	RebaseFix
	// SecurityFix moves a package's open Dependabot alerts to a patched version
	// in a fresh branch (ADR-0040).
	SecurityFix
	// Triage judges whether one backlog ticket is ready for an agent to work
	// unattended, read-only (ADR-0045).
	Triage
)

// Resume says how an Implement session enters its worktree. The host picks it;
// only the worktree instruction differs between them.
type Resume int

const (
	// Fresh points the agent at the worktree the host just created.
	Fresh Resume = iota
	// ResumedBranch is for a ticket whose own branch already carries un-merged
	// commits for it: inspect them first, prefer verify-and-handoff (BEH-554).
	ResumedBranch
	// AfterRefusal is the retry after a usage-policy refusal: the prior attempt's
	// uncommitted work survives at Context.WorktreePath (BEH-389).
	AfterRefusal
)

// Context is everything a Stage prompt is composed from. Body is the Consumer's
// half of the ADR-0009 split — the only field a Consumer supplies; everything else
// feeds the harness-owned envelope. Fields a Stage does not use are ignored.
type Context struct {
	Ticket       ticket.Ticket
	Slug         string
	BranchPrefix string
	WorktreePath string
	// Tracker is the configured tracker kind (`linear`, `github`, `jira`). The
	// envelope's tracker-off steer names it and forbids the tool that would reach
	// it (ADR-0010); empty names no tracker in particular.
	Tracker string

	// Body is the Consumer-supplied prompt body from `.agent-harness/prompts/`.
	// CIFix and RebaseFix invoke no skill and ignore it.
	Body string

	// Base is the commit the branch was cut from or last rebased onto — its
	// merge-base with origin/main, resolved host-side. The Review and Retrospective
	// envelopes scope the diff to it (#86); empty falls back to origin/main.
	Base string

	// LogDir is the run's log dir, absolute (`<checkout>/.agent-harness/logs/<Identifier>`
	// for a ticket, the loop's own dir for a PR loop's run): the session starts in
	// the worktree, not in the checkout a relative path would resolve from (#245). Transcripts names the files the run left there, which the
	// Retrospective envelope lists so a Consumer body cannot point it elsewhere (#185).
	LogDir      string
	Transcripts []string

	// Sentry is what the host fetched of the Sentry issues the ticket cites (#211,
	// ADR-0041). The Implement and Review envelopes carry it after the ticket
	// context; nil (no `[sentry]` table) leaves the prompt as it was.
	Sentry *sentry.Snapshot

	// Resume selects the Implement variant.
	Resume Resume
	// Filed lists the finding classes already filed, for a Retrospective re-run.
	Filed []FiledFinding
	// PriorReview is the verdict a prior review emitted over exactly the commits
	// now on the branch, for a Review re-run (#44); nil reviews cold.
	PriorReview *PriorReview
	// RedGate is the host gate that went red over the review's own commit, for
	// the one Review re-launch that follows it (#111); nil on a first review.
	RedGate *RedGate
	// Resolution is what a pre-push conflict resolution wrote beyond the files git
	// flagged, for the scoped review of it that runs before the push (#114); nil on
	// every other review.
	Resolution *Resolution
	// CILogs is the failing job log the host fetched for CIFix; CILogAvailable
	// says whether it is a usable log or only an error marker (BEH-558).
	CILogs         string
	CILogAvailable bool

	// Branch, when set, names the branch the session works on in place of
	// `<BranchPrefix>/<Slug>`: the PR loop fixes a branch it did not name (ADR-0019).
	// CIFix, RebaseFix and the Review footer name it, and a body reads it as
	// `{{.Branch}}` (#186).
	Branch string
	// Checkpoint is the recovery checkpoint a PR loop's fix session starts from:
	// an earlier attempt's uncommitted work, saved when its session ended (#246).
	// CIFix and RebaseFix name it as unverified; nil starts from origin's tip.
	Checkpoint *Checkpoint
	// Merge selects the RebaseFix variant that merges origin/main into the branch
	// instead of replaying it, for a branch a human opted in: its history must not
	// be rewritten (ADR-0019).
	Merge bool

	// Alerts are the open Dependabot alerts on one package that a SecurityFix
	// session fixes together (ADR-0040, decision 1).
	Alerts []github.Alert
}

// branch is the branch the session's worktree is on: the override, else the
// canonical `<BranchPrefix>/<Slug>`.
func (c Context) branch() string {
	if c.Branch != "" {
		return c.Branch
	}
	return c.BranchPrefix + "/" + c.Slug
}

// For composes the `-p` prompt for Stage s: the envelope for that Stage wrapped
// around ctx.Body. It panics on an unknown Stage, which is a programming error.
func For(s Stage, ctx Context) string {
	switch s {
	case Implement:
		return implement(ctx)
	case Review:
		return review(ctx)
	case Retrospective:
		return retrospective(ctx)
	case CIFix:
		return ciFix(ctx)
	case RebaseFix:
		return rebaseFix(ctx)
	case SecurityFix:
		return securityFix(ctx)
	case Triage:
		return triage(ctx)
	}
	panic(fmt.Sprintf("prompt: unknown stage %d", s))
}

// bodyFields is the template context available to a Consumer prompt body. A body
// interpolates these with `{{.Field}}` (text/template) so herd's committed bodies
// can name the ticket, slug, branch, and worktree without the harness hardcoding
// them. Branch is the branch the worktree is really on (Context.branch): a PR-loop
// slug is `pr-<n>`, so `{{.BranchPrefix}}/{{.Slug}}` names a branch that does not
// exist there (#186).
//
// It is a map, not a struct, so a field this binary does not know renders empty
// instead of failing the whole body (#219): the body comes from the checkout, the
// binary may be older, and a struct turned one unknown field into no interpolation
// at all.
func bodyFields(c Context) map[string]string {
	return map[string]string{
		"Identifier":   c.Ticket.Identifier,
		"Title":        c.Ticket.Title,
		"Slug":         c.Slug,
		"BranchPrefix": c.BranchPrefix,
		"Branch":       c.branch(),
		"WorktreePath": c.WorktreePath,
		"LogDir":       c.LogDir,
	}
}

// missingKey pulls the key out of text/template's missingkey=error failure.
var missingKey = regexp.MustCompile(`map has no entry for key "([^"]*)"`)

// renderBody interpolates the Consumer body's `{{.Field}}` placeholders against
// the Context. A body is Consumer-controlled and may be malformed or empty; rather than
// fail the whole prompt, a placeholder this binary does not know renders empty and
// a body that will not parse or execute goes out raw — quality is the Consumer's
// concern, the contract (which is envelope, not body) is never at risk. Neither is
// silent: the warnings name each unknown placeholder, or why the body went out
// raw, for the launch to narrate (BodyWarnings).
func renderBody(c Context) (string, []string) {
	tmpl, err := template.New("body").Option("missingkey=error").Parse(c.Body)
	if err != nil {
		return c.Body, []string{rawBodyWarning(err)}
	}
	fields := bodyFields(c)
	var warnings []string
	// Each pass learns one unknown key and renders it empty on the next, so the
	// loop is bounded by the body's distinct placeholders.
	for {
		var b strings.Builder
		err := tmpl.Execute(&b, fields)
		if err == nil {
			return b.String(), warnings
		}
		m := missingKey.FindStringSubmatch(err.Error())
		if m == nil {
			return c.Body, append(warnings, rawBodyWarning(err))
		}
		if _, seen := fields[m[1]]; seen {
			return c.Body, append(warnings, rawBodyWarning(err))
		}
		fields[m[1]] = ""
		warnings = append(warnings, fmt.Sprintf("prompt body names {{.%s}}, which this harness does not know — it renders empty; the body may be newer than the harness binary (#219)", m[1]))
	}
}

func rawBodyWarning(err error) string {
	return "prompt body was not rendered, so it goes out with its placeholders as literal text (" + err.Error() + ") (#219)"
}

// body is the rendered Consumer body, for the stage composers.
func body(c Context) string {
	s, _ := renderBody(c)
	return s
}

// BodyWarnings is what rendering c.Body would warn of: each placeholder this
// binary does not know, or why the body could not be rendered at all (#219). The
// caller narrates them at launch; nil means the body rendered cleanly.
func BodyWarnings(c Context) []string {
	_, w := renderBody(c)
	return w
}

// defang neutralizes the pinned Claude CLI's `!`…“ inline-bash directive inside
// UNTRUSTED, externally-sourced prompt text — ticket titles/bodies, inlined
// sub-issue specs, prior retrospective findings, and fetched CI logs. When
// `claude -p` sees a `!` immediately followed by a backtick, it treats the
// backtick-delimited text as a shell command, runs it host-of-sandbox, and — when
// that command errors (e.g. a stray “ !`/` “ runs `/`, "Is a directory") — the
// WHOLE session degenerates to a zero-turn no-op: no model call, no worktree, yet
// it exits 0. The harness reads that as "environmental crash before any work"
// (BEH-543), retries, and after three such tickets the circuit breaker trips and
// the loop winds down. So any ticket whose body merely CONTAINS a `!`…“ sequence
// silently kills its own implementation session and can strand the whole daemon.
// We break the `!`+backtick adjacency with a zero-width space (U+200B): invisible
// to the model reading the prompt (the text still renders as `!`cmd“), but no
// longer a literal `!“ for the CLI's directive matcher. Only external text is
// defanged; the harness-owned envelope prose is trusted and left byte-exact.
func defang(s string) string {
	return strings.ReplaceAll(s, "!`", "!"+"\u200b"+"`")
}

// join assembles a prompt from its sections, dropping empties and separating the
// rest with a blank line — the format every body-carrying Stage shares.
func join(sections ...string) string {
	kept := make([]string, 0, len(sections))
	for _, s := range sections {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}

// ticketContext renders the harness-injected ticket block. The sandbox cannot
// reach the tracker (ADR-0001/0002), so the envelope injects the title, body, and
// any inlined sub-issue specs the session would otherwise try to fetch.
func ticketContext(t ticket.Ticket) string {
	s := join(
		"Ticket context (already fetched for you — do not look it up):",
		"# "+t.Identifier+": "+defang(t.Title)+"\n\n"+defang(t.Description),
	)
	if sub := subIssuesSection(t.SubIssues); sub != "" {
		s = join(s, sub)
	}
	return s
}

// trackerNames maps a tracker kind to the name the envelope shows the agent.
var trackerNames = map[string]string{
	"linear": "Linear",
	"github": "GitHub Issues",
	"jira":   "Jira",
}

// trackerTools names, per tracker kind, the in-sandbox route that would reach it
// and so must be forbidden by name: the Linear MCP the stock skills call, and the
// `gh` CLI the stock skills reach for on a GitHub-tracked repo. The sandbox holds
// no tracker credential (ADR-0002), so this is a steer, not the enforcement. Jira
// has no such route.
var trackerTools = map[string]string{
	"linear": "do not call any `mcp__linear-server__*` tool, ",
	"github": "do not run `gh issue`, ",
}

// trackerOff is the envelope's tracker-off steer, shared by every Stage: the
// harness performs every tracker interaction host-side and the sandbox touches
// none (ADR-0010). It names the configured tracker — never Linear by default —
// so a GitHub- or Jira-tracked agent is steered off the tracker it actually has
// (#35). An unknown kind reads as itself; an empty one names no tracker.
func trackerOff(kind string) string {
	subject := "the issue tracker"
	if kind != "" {
		name, ok := trackerNames[kind]
		if !ok {
			name = kind
		}
		subject += " (" + name + ")"
	}
	return "Do NOT touch " + subject + ": " + trackerTools[kind] + "do not move the ticket, open, or comment on issues. The harness owns all tracker I/O (ADR-0010)."
}

// trackerOffSteer is the envelope's tracker-off contract for the implementation
// stage: the ticket is already claimed host-side and the sandbox must not touch
// the tracker.
func trackerOffSteer(id, kind string) string {
	return id + " is already claimed and moved to In Progress for you. " + trackerOff(kind)
}

// findingsDropboxProtocol is the envelope's findings-dropbox contract for the
// implementation stage: harness/environment friction goes to `/findings/out.json`
// in the `{title, body, kind, key}` shape the host-side filer reads, never to the
// tracker directly.
const findingsDropboxProtocol = "If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file issues in the tracker yourself. Instead append them to `/findings/out.json` as a JSON array of `{title, body, kind, key}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem). The harness reads this file after the session and files the issues for you, skipping any whose key already has an open issue. If you have no findings, leave the file untouched."

// branchHandoffContract is the envelope's branch/handoff contract: the whole
// pipeline (verify, push, PR, CI, review, rebase) keys off the canonical
// `<prefix>/<slug>` branch, so a handoff committed anywhere else is silently
// stranded (BEH-615). It also keeps origin/main read-only: the harness rebases
// the branch before push and runs its own conflict-resolution session, so a
// session that syncs onto a moved main itself only adds turns and a hand-rebuilt
// commit (#133). It is appended structurally, so a body cannot drop it.
func branchHandoffContract(branchPrefix, slug string) string {
	return "The harness keys its handoff check, push, PR, and review off the canonical `" + branchPrefix + "/" + slug + "` branch, so committing to any other branch (e.g. `fix/" + slug + "`) silently strands your work where the harness never sees it. Do not rebase or merge `origin/main` into the branch; the harness rebases it before push and resolves conflicts in its own session. Fetch `origin/main` only to read it (e.g. the next free ADR number)."
}

// freshWorktreeInstr is the implementation stage's worktree instruction for a
// fresh run. The harness now creates the worktree + canonical branch host-side
// (ADR-0008/BEH-636), so this points the agent AT that pre-created worktree and
// forbids running `new-worktree.sh` — it never creates one itself. It is envelope
// content (not body) because it varies per implementation variant (fresh /
// resumed-branch / usage-policy retry), each of which enters the worktree
// differently.
func freshWorktreeInstr(slug, worktreePath, branchPrefix string) string {
	branch := branchPrefix + "/" + slug
	return "The worktree for slug `" + slug + "` has already been created for you at `" + worktreeAt(slug, worktreePath) + "` on branch `" + branch + "` — the harness owns worktree and branch creation, so do NOT run `scripts/new-worktree.sh` and do NOT create a new worktree or branch. `cd` into that worktree and do all your work there, committing your handoff on `" + branch + "` verbatim regardless of the ticket type (bug-fix, chore, docs, …)."
}

// worktreeAt is where the envelope says the worktree is: its absolute path, since
// the session starts in the worktree rather than in the checkout a relative path
// resolves from (#245). A Context built without one — a test's — falls back to
// the path relative to the checkout.
func worktreeAt(slug, worktreePath string) string {
	if worktreePath != "" {
		return worktreePath
	}
	return ".claude/worktrees/" + slug
}

// tddFooter is the shared contract footer for every implementation-stage prompt
// (fresh, resumed-branch, retry). It structurally guarantees the four
// non-overridable envelope items: branch/handoff contract, tracker-off steer,
// findings-dropbox protocol, and bash-quirk steer.
func tddFooter(id, kind, branchPrefix, slug string) string {
	return join(
		branchHandoffContract(branchPrefix, slug),
		trackerOffSteer(id, kind),
		findingsDropboxProtocol,
		bashQuirkSteer,
	)
}

// implement composes an implementation-stage prompt: the Consumer body (which
// invokes the skill, e.g. `/tdd`, and carries project conventions), the
// Resume-specific worktree instruction, the injected ticket context, and the
// non-overridable contract footer. The Resume variants differ only in the
// worktree instruction.
func implement(c Context) string {
	var worktreeState string
	switch c.Resume {
	case ResumedBranch:
		worktreeState = resumedBranchWorktreeState(c.Slug, c.WorktreePath, c.BranchPrefix)
	case AfterRefusal:
		worktreeState = resumeWorktreeState(c.Slug, c.WorktreePath, c.BranchPrefix)
	default:
		worktreeState = freshWorktreeInstr(c.Slug, c.WorktreePath, c.BranchPrefix)
	}
	return join(
		body(c),
		worktreeState,
		ticketContext(c.Ticket),
		sentrySection(c.Sentry),
		prerequisitesSection(c.Ticket.Prerequisites),
		openQuestionsSection(c.Ticket.OpenQuestions()),
		adrNumberSection(c.Ticket.ADRFiles()),
		"---",
		tddFooter(c.Ticket.Identifier, c.Tracker, c.BranchPrefix, c.Slug),
	)
}

// prerequisitesSection names each still-open PR the ticket builds on (#82).
// Selection holds such a ticket, so this is the dispatched-anyway case: a
// hand-passed run, or a PR opened after selection. Without it the session had to
// search every ref for the sibling branch, then judged the two PRs disjoint when
// both edited one paragraph of a shared doc, and the sibling's merge left the
// shipped PR conflicting with base. Empty when there is no open prerequisite.
func prerequisitesSection(prs []ticket.Prerequisite) string {
	if len(prs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("This ticket builds on work that is not yet merged — `main` does not have it, and will change underneath you when it merges:")
	for _, p := range prs {
		// Defang the whole line, not just the title: a head branch is PR-author text
		// too, and one ending in `!` would form `!`` against the backtick after it.
		b.WriteString(defang("\n\n- " + p.Identifier + " " + p.Title + " (" + p.URL + ") on branch `" + p.Branch + "` (`origin/" + p.Branch + "`); read its version of a file with `git show origin/" + p.Branch + ":<path>`."))
	}
	b.WriteString("\n\nDo not assume your PR and that one are disjoint. For any shared file — a design doc, an ADR, `CONTEXT.md`, a README — either base your edit on that branch's version of the file, or leave the shared file alone and say so in your handoff. If the branch ref is not in your checkout, leave shared docs alone.")
	return b.String()
}

// openQuestionsSection names each section of the ticket body that still lists
// questions for a human (#90). The ticket carries a ready label anyway, so this
// is a dispatched-anyway case like a prerequisite. The #55 session met "Open
// questions (for a human before this is agent-ready)" and had to choose, unaided,
// between stopping and deciding them. Empty when the body has no such section.
func openQuestionsSection(headings []string) string {
	if len(headings) == 0 {
		return ""
	}
	quoted := make([]string, len(headings))
	for i, h := range headings {
		quoted[i] = `"` + h + `"`
	}
	return defang("The ticket body still has a section of questions meant for a human to settle before an agent picked it up: " + strings.Join(quoted, ", ") + ". It was labelled ready anyway, so do not answer them silently. If the ticket can be implemented without settling a question, leave it open and say so in your handoff. If it cannot, make the smallest decision you can defend, record it and why (in an ADR when it is a design choice), and list every decision you made in your handoff and commit body so a human reviews it.")
}

// adrNumberSection names each numbered ADR file the ticket names (#228). A
// ticket fixes a new ADR's number when it is drafted, and a sibling may take it
// before the ticket runs: #197 asked for docs/adr/0039-<slug>.md when origin/main
// already had ADR-0039. A session that wrote the filename literally collides, and
// only check-adr catches it. The number of a new ADR is the next free one on
// origin/main (ADR-0034). Empty when the ticket names no ADR file.
func adrNumberSection(files []string) string {
	if len(files) == 0 {
		return ""
	}
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = "`" + f + "`"
	}
	return defang("The ticket names ADR files by number: " + strings.Join(quoted, ", ") + ". An ADR that already exists keeps its number. For a new ADR the number is a guess made when the ticket was drafted, and a sibling may have taken it since: give it the next free number on origin/main (`git fetch`, then read `origin/main`'s ADR directory), whatever the ticket says (ADR-0034), and if that differs from the ticket's, say which number shipped in your commit body.")
}

// subIssuesSection renders the inlined child sub-issue specs for an umbrella/batch
// ticket (BEH-619). An umbrella defers its real work to sub-issues, but the sandbox
// is isolated from the tracker (ADR-0002): mid-session the agent can't fetch a
// child's spec, so it reaches for an unavailable tracker call, then guesses or
// defers and under-delivers (the BEH-520 run shipped 1 of ~9 children). The harness
// fetches each child host-side and inlines its full title + body here, clearly
// delimited with its id. Empty when the ticket has no sub-issues.
func subIssuesSection(subs []ticket.SubIssue) string {
	if len(subs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("This is an umbrella/batch ticket: its real work lives in the sub-issues below, whose full specs are inlined here (already fetched for you — the sandbox CANNOT reach the issue tracker, so do NOT try to look them up). Implement EVERY sub-issue, not just the umbrella body above; if you defer one, say which and why in your handoff:")
	for _, s := range subs {
		b.WriteString("\n\n### " + s.Identifier + ": " + defang(s.Title))
		if strings.TrimSpace(s.Description) != "" {
			b.WriteString("\n\n" + defang(s.Description))
		}
	}
	return b.String()
}

// resumedBranchWorktreeState is the worktree instruction the host swaps in for the
// fresh one when the dispatched ticket's OWN feature branch already carries
// un-merged commits referencing it (ResumedBranchAdvisory fired — BEH-554). A
// prior session resumed that worktree and likely already landed a complete fix, so
// this steers the agent to inspect the branch's existing commits FIRST and prefer
// verify-and-handoff over re-implementing.
func resumedBranchWorktreeState(slug, worktreePath, branchPrefix string) string {
	branch := branchPrefix + "/" + slug
	return join(
		"Enter the worktree already created for you at `"+worktreeAt(slug, worktreePath)+"` on branch `"+branch+"` — the harness owns worktree and branch creation, so do NOT run `scripts/new-worktree.sh`.",
		"IMPORTANT — RESUMED WORKTREE: branch `"+branch+"` ALREADY carries commit(s) for this ticket ahead of `main` from a previous session — the fix may already be COMPLETE. Before you plan or write any code, enter the worktree and inspect that history: run `git log origin/main..HEAD` (not local `main`, which the harness never updates and may be stale) and read the diff. If the ticket's behaviour is already implemented and tested on the branch, do NOT re-implement, rewrite, or redo it — instead verify the gates pass and record \"already fixed on branch — recommend review/handoff\" in your handoff, then stop. Only finish or extend the work if the branch's fix is genuinely incomplete. The ticket prose below may describe the code as still un-fixed even though the branch already resolves it, so trust the branch's git history over the prose.",
	)
}

// resumeWorktreeState is the worktree instruction for a *retry* of the
// implementation session after a usage-policy refusal (BEH-389). The first attempt
// already created the worktree and left its work uncommitted on disk (the
// real-path bind mount survives the refusal), so this steers the agent to RESUME
// that existing worktree and commit the surviving work rather than recreate it.
func resumeWorktreeState(slug, worktreePath, branchPrefix string) string {
	return "IMPORTANT: a previous attempt was interrupted (a usage-policy refusal), but its work survives. The worktree ALREADY EXISTS at `" + worktreePath + "` on branch `" + branchPrefix + "/" + slug + "`, and it likely holds uncommitted changes from that attempt. Do NOT create a new worktree and do NOT run `new-worktree.sh` — that would fail on the already-existing branch. `cd` into the existing worktree, inspect what's there with `git status`/`git diff`, finish anything incomplete, run the gates, and commit the handoff. Recovering and committing that surviving diff is the whole point of this retry."
}

// FiledFinding is an already-filed harness-improvement finding class surfaced to
// a retrospective re-run so the session treats it as settled instead of
// re-deriving it (BEH-539). Key is the stable failure-class slug (may be empty
// for a title-only finding); Title is the human summary for the prompt list.
type FiledFinding struct {
	Key   string
	Title string
}

// PriorReview is the verdict a prior review session emitted, handed to a Review
// re-run whose branch has not moved since (#44). Head is the commit that review
// covered — the range is `origin/main..Head` — and Disposition is its verdict line
// verbatim ("Disposition: clear — <reason>").
type PriorReview struct {
	Head        string
	Disposition string
}

// RedGate is a host gate that went red over the commit a review session made,
// handed to the one re-launch of that review (#111). Name is the Consumer's gate
// name and Log the tail of its step log.
type RedGate struct {
	Name string
	Log  string
}

// Checkpoint is a harness recovery checkpoint a fix session's worktree starts from
// (#246): Commit is the checkpoint, on top of Base, origin's tip of the branch.
type Checkpoint struct {
	Commit string
	Base   string
}

// checkpointSteer tells a fix session its worktree starts from an earlier
// attempt's saved, unverified work rather than origin's tip (#246). In herd
// PR-1384 that saved diff was the wrong fix: a `waitFor` that hung the story
// runner. Empty when there is no checkpoint.
func checkpointSteer(cp *Checkpoint) []string {
	if cp == nil {
		return nil
	}
	c, b := shortRef(cp.Commit), shortRef(cp.Base)
	return []string{
		"IMPORTANT — UNVERIFIED PRIOR WORK: an earlier attempt at this PR was cut off (a time cap, a crash) before it committed its change. The harness saved what it left uncommitted as the recovery checkpoint commit `" + c + "` on top of origin's tip `" + b + "`, and this worktree starts from that checkpoint, not from origin's tip. Nothing has checked it: no gate ran over it, the session that wrote it never finished, and it can be wrong — an earlier saved diff was a fix that made the test it fixed hang.",
		"",
		"Read it before you build on it: `git show " + c + "`. Keep it only if you can confirm it is right, and verify it as you would your own change; the harness re-runs every gate over whatever you commit before it pushes. If it is wrong, or you cannot tell, drop it before you start: `git reset --hard " + b + "` puts the branch back on origin's tip and loses only the unpushed checkpoint, so that reset is allowed even where this prompt forbids rewriting history.",
		"",
	}
}

// withCheckpoint puts the checkpoint steer right after a fix prompt's opening
// paragraph (lines[0] and its blank line), before the session reads its job.
func withCheckpoint(c Context, lines []string) []string {
	steer := checkpointSteer(c.Checkpoint)
	if steer == nil {
		return lines
	}
	return append(append(append([]string{}, lines[:2]...), steer...), lines[2:]...)
}

// shortRef is a commit's abbreviated SHA, as a prompt names it.
func shortRef(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Resolution is what a pre-push conflict-resolution session wrote after the branch
// passed review, handed to the scoped review of it (#114). ReviewedBase..ReviewedHead
// is the range the first review cleared; Conflicted are the files git flagged on
// the replay; Beyond are the files the resolution changed outside them; Messages
// says it also reworded commit messages; RangeDiff is `git range-diff` from the
// reviewed range to the resolved one.
type Resolution struct {
	ReviewedBase string
	ReviewedHead string
	Conflicted   []string
	Beyond       []string
	Messages     bool
	RangeDiff    string
}

// rangeDiffMaxLines bounds how much of the range-diff the scoped review is handed
// inline; the session can run the command itself for the rest.
const rangeDiffMaxLines = 400

// retroFindingsProtocol is the retrospective stage's findings-dropbox contract:
// unlike the implementation stage, the retrospective's ONLY output is findings, so
// it must always write the file — an absent file means the step never ran.
const retroFindingsProtocol = "Write your findings to `/findings/out.json` as a JSON array of `{title, body, kind, key, audience, needs_human}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem, so the harness skips a finding whose key already has an open issue; audience is covered in the classification rule below; needs_human is `true` only when an agent cannot do the fix alone — it needs a person's decision between designs, a credential or secret, or a step in a dashboard or other UI — and `false` otherwise, because the harness marks every other finding ready for an agent to work unattended). **Always write the file**, even when you found nothing — write an empty array `[]` in that case. An absent file means the step never ran, so never end without writing it."

// retroAudienceContract is the retrospective envelope's non-overridable
// classify-at-source + sanitize rule (ADR-0011). Every finding carries an
// audience so the harness routes it: a "project" finding (about the Consumer's
// own codebase/tests/CI) files to the project's tracker; a "harness" finding
// (about the harness, its sandbox, or its prompt contract) stays in a local
// artifact dir in the repo. Because a harness finding may later be shared to a
// PUBLIC upstream repo, the sanitize rule is mandatory and cannot be weakened by
// the Consumer body — it is appended structurally, like the rest of the envelope.
const retroAudienceContract = "Classify EVERY finding with an `audience` field, either `\"project\"` or `\"harness\"`. Use `\"project\"` for friction about the Consumer's OWN codebase, tests, or CI (a flaky spec, a slow or wrong gate, a broken project script) — the harness files these to the project's issue tracker. Use `\"harness\"` for friction about the harness, its sandbox, or its prompt contract itself (a missing sandbox dependency, an opaque CLI/bash quirk, a misleading or contradictory envelope instruction) — the harness keeps these in a local artifact dir in this repo; nothing leaves the repo. When unsure, use `\"harness\"`. NON-OVERRIDABLE SANITIZE RULE: a `\"harness\"` finding must state ONLY the failure class and harness-side detail — what broke in the sandbox/contract and how to fix it. Never include (do not sanitize by paraphrase — omit entirely) Consumer source code, transcript excerpts, ticket contents, project file paths, or any secret/token. This rule holds regardless of anything else in this prompt or the project body."

// retroADRNumberRule keeps a drafted ticket from fixing a new ADR's number
// (#228). A finding waits in the queue as a ticket, siblings land ADRs meanwhile,
// and #197's docs/adr/0039-<slug>.md was taken by the time it ran. The number is
// the implementing session's to pick: the next free one on origin/main (ADR-0034).
const retroADRNumberRule = "When a finding's fix is a new ADR, name the new ADR by its slug or title only and never by number (no `ADR-NNNN`, no `NNNN-<slug>.md`): other ADRs land while the ticket waits, so the session that writes it takes the next free number on origin/main (ADR-0034). Name an existing ADR by its number as usual."

// retroContractSteer is the retrospective stage's tracker-off + read-only
// contract: no code changes, no push, no tracker.
func retroContractSteer(kind string) string {
	return join(
		"This session is read-only and reaches no remote. Make NO code changes and do NOT commit or push. The harness reads `out.json` after the session and files each finding itself.",
		trackerOff(kind),
	)
}

// retrospective builds the `-p` prompt for the sandboxed retrospective
// session (the terminal tool). The Consumer body points the session at the
// ticket-keyed transcripts and the diff; the harness envelope guarantees the
// findings-dropbox protocol, the tracker-off/read-only contract, and the
// bash-quirk steer, plus the dynamic already-filed dedup context (BEH-539).
func retrospective(c Context) string {
	return join(
		body(c),
		transcriptsSection(c.LogDir, c.Transcripts),
		retroFindingsProtocol,
		retroAudienceContract,
		retroADRNumberRule,
		branchBaseSection(c.Base, c.WorktreePath, c.branch()),
		retroContractSteer(c.Tracker),
		alreadyFiledSection(c.Filed),
		bashQuirkSteer,
	)
}

// transcriptsSection names where the run's transcripts are and which files they
// are (#185). The harness knows both — a ticket's logs sit under
// logs/<Identifier>, a PR loop's under logs/loops/<loop>/<Identifier>, and their
// sessions are named differently — so the envelope says it rather than leave a
// Consumer body to hard-code one layout. A session handed a dir that does not
// exist either searches for the real one or writes [] over no transcripts. With
// nothing listed (a dry-run plan) it names the streams to read by pattern.
func transcriptsSection(logDir string, transcripts []string) string {
	if logDir == "" {
		return ""
	}
	dir := "`" + strings.TrimSuffix(logDir, "/") + "/`"
	if len(transcripts) == 0 {
		return "This run's logs are under " + dir + ": read every `*.jsonl` stream there except your own `retrospective-*` one, and the `*.log` step logs beside them."
	}
	var b strings.Builder
	b.WriteString("This run's logs are under " + dir + ". Read every one of them (your own `retrospective-*` transcript is left out):")
	for _, name := range transcripts {
		b.WriteString("\n  - `" + name + "`")
	}
	return b.String()
}

// branchBaseSection scopes a Review or Retrospective session's view of the branch
// to the commits this ticket added (#86). The harness cuts and rebases the branch
// on origin/main but never moves local `main`, so once a sibling ticket merges
// mid-run `main...HEAD` also carries that sibling's commits and the session has to
// work out on its own that the foreign work is not the branch's. Naming the exact
// base removes the question; an unresolved base falls back to origin/main, whose
// three-dot diff finds the same fork point.
//
// The session's cwd is the primary checkout, not the worktree, and that checkout
// can sit on an unrelated branch that also descends from the base, so a bare
// `<base>..HEAD` there quietly shows a different change (#220). The range runs in
// the worktree with `git -C`, and a line names the worktree and its real branch,
// so neither depends on a Consumer body that may not name them or may go out raw.
// An unknown worktree (a test, a plan) keeps the bare range.
func branchBaseSection(base, worktree, branch string) string {
	if base == "" {
		base = "origin/main"
	}
	git, where := "git ", ""
	if worktree != "" {
		git = "git -C " + shellWord(worktree) + " "
		where = "This run's worktree is `" + worktree + "` on branch `" + branch + "`; your cwd may be the primary checkout, which can be on another branch, so read the branch's change in the worktree, never with a bare `git` in the cwd. "
	}
	return where + "The branch's change is exactly `" + git + "log " + base + "..HEAD` and `" + git + "diff " + base + "...HEAD`" +
		" — `" + base + "` is the commit the harness cut the branch from or last rebased it onto. Scope the change with that range only." +
		" Do NOT diff against local `main`: the harness never updates it, so it may be stale and `main...HEAD` would also show commits from other, already-merged tickets."
}

// shellWord single-quotes s for a shell command line when it carries a character
// the shell would split or expand; a plain path goes out as it is.
func shellWord(s string) string {
	if !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()<>|&;~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// alreadyFiledSection renders the dedup context (BEH-539): the open finding
// classes the harness has already filed — across all tickets, not just this one
// (#65) — so the session treats them as settled and spends its budget on NEW
// friction instead of re-deriving them, and reuses a listed key verbatim for the
// same class so key dedup fires instead of a drifted slug filing a duplicate.
// Empty when there's nothing already filed.
func alreadyFiledSection(filed []FiledFinding) string {
	if len(filed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The harness has ALREADY filed the open finding classes below, across all tickets (this one and others), so treat them as SETTLED. Do NOT re-investigate or re-derive them; spend your budget only on NEW friction. If a finding of yours is the same failure class as one listed — even worded differently — reuse its key EXACTLY (copy the slug verbatim, do not coin a variant) so the harness dedups it against the open one instead of filing a duplicate under a new key:")
	for _, f := range filed {
		b.WriteString("\n  - ")
		if f.Key != "" {
			b.WriteString(defang(f.Key) + " — ")
		}
		b.WriteString(defang(f.Title))
	}
	return b.String()
}

// reviewContractFooter is the review stage's contract footer. It structurally
// guarantees the review analog of the four envelope items: the branch/handoff
// contract (commit locally, the harness owns push/PR — ADR-0002 — and rebases
// first, so a commit message names commits by subject, not SHA — #95), the tracker-off
// steer (ADR-0010), the findings protocol (review emits NONE; retrospective owns
// findings), and the bash-quirk steer.
func reviewContractFooter(kind, branch string) string {
	return join(
		"Apply any review fixes as a LOCAL commit on `"+branch+"` and stop there; a review that finds nothing to fix makes no commit. Do NOT push, do NOT run `gh`, do NOT open or raise a PR. The harness owns all remote git I/O (ADR-0002): it independently re-runs the quality gates host-side and, only if they pass, pushes the branch and opens the PR itself.",
		"The host gate is the only thing that authorises the push, and a red host gate over a commit you made returns to you once with its output: the harness re-launches this review with the failing gate's name and log tail, then re-runs the gates. A gate still red after that, or red over a commit you did not make, fails the ticket.",
		"Before it pushes, the harness rebases the branch onto origin/main, which rewrites every commit SHA on it. So do NOT name a commit by its SHA in your commit message; name the commit your fixes amend by its subject line. A SHA in the pushed message would point at a commit that is not on the branch or in the PR.",
		trackerOff(kind),
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool, running last over every transcript, owns findings now — this is a deliberate change from the skill's default. Your only outputs are your report and any review fixes committed locally.",
		bashQuirkSteer,
	)
}

// review builds the `-p` prompt for the sandboxed review session (the second
// tool). The Consumer body invokes the review skill and carries the cold-review /
// lenses-first / disposition conventions; the harness envelope injects the ticket
// context (the intent to review against) and the non-overridable contract footer.
func review(c Context) string {
	return join(
		body(c),
		ticketContext(c.Ticket),
		sentrySection(c.Sentry),
		branchBaseSection(c.Base, c.WorktreePath, c.branch()),
		priorReviewSection(c.PriorReview),
		redGateSection(c.RedGate),
		resolutionSection(c.Resolution),
		"---",
		reviewContractFooter(c.Tracker, c.branch()),
	)
}

// priorReviewSection tells a Review re-run that the diff it is handed was already
// reviewed (#44). Without it a re-run whose implementation added no commits got the
// same cold envelope as a first pass, re-derived a verdict that already existed, and
// — to have something to fix — stacked a cosmetic "Review fixes" commit on the last
// one. Empty when there is no prior verdict: a first review reads as it always did.
func priorReviewSection(p *PriorReview) string {
	if p == nil {
		return ""
	}
	return "IMPORTANT — UNCHANGED SINCE THE LAST REVIEW: a prior review of this ticket already covered exactly the commits now on the branch (`origin/main.." + p.Head + "`, HEAD `" + p.Head + "`) and emitted:\n\n    " + defang(p.Disposition) + "\n\nNo commit has landed since. Confirm HEAD is still `" + p.Head + "` (`git rev-parse HEAD`); if it is not, ignore this note and review cold. If it is, do NOT re-review the diff from scratch and do NOT commit just to have something to show: run the gate, then emit your `## Review:` report and re-emit that same Disposition line. Commit a fix ONLY for a real defect — a spec, correctness or invariant failure, not a cosmetic tidy-up — and name it in the report."
}

// redGateSection hands the re-launched review the host gate that went red over
// the commit the previous session made (#111). The log is the gate's own output,
// so it is defanged like any other text the agent did not write.
func redGateSection(g *RedGate) string {
	if g == nil {
		return ""
	}
	return "IMPORTANT — THE HOST GATE IS RED OVER YOUR LAST COMMIT: a previous review session of this ticket committed a fix, and the harness's host-side re-run of gate `" + defang(g.Name) + "` then failed over it. This is that session's one re-launch. The tail of the gate's log:\n\n```\n" + defang(g.Log) + "\n```\n\nFix what the gate reports as a LOCAL commit, re-run that one gate to confirm it, then emit your `## Review:` report as usual. The harness re-runs every gate after you stop; if it is still red the ticket fails, so do not stop on a guess."
}

// resolutionSection scopes a review to what a pre-push conflict resolution wrote
// after the branch passed review (#114). The resolution session is the one stage
// that writes code after review, and a green gate cannot tell whether a test it
// ported still asserts what its author meant, so its edits beyond the conflicted
// files get a review of their own, and only those.
func resolutionSection(r *Resolution) string {
	if r == nil {
		return ""
	}
	reworded := ""
	if r.Messages {
		reworded = " It also reworded the branch's commit message(s)."
	}
	conflicted := "a file git flagged"
	if len(r.Conflicted) > 0 {
		conflicted = codeList(r.Conflicted)
	}
	rangeDiff := r.RangeDiff
	if lines := strings.Split(rangeDiff, "\n"); len(lines) > rangeDiffMaxLines {
		rangeDiff = strings.Join(lines[:rangeDiffMaxLines], "\n") + "\n… (truncated — run the command for the rest)"
	}
	return "IMPORTANT — A SCOPED REVIEW OF A CONFLICT RESOLUTION: this branch already passed review as `" + r.ReviewedBase + ".." + r.ReviewedHead + "`. Before the harness could push it, origin/main had moved, and replaying the branch onto it conflicted in " + conflicted + ". A conflict-resolution session resolved that, and it also changed " + codeList(r.Beyond) + ", which git did not flag." + reworded + " The gates are green over the resolved tree, but nobody has reviewed those changes." +
		"\n\nReview ONLY what the resolution wrote, set against the range review cleared — not the rest of the branch. In particular: does a test it ported or edited still assert what its author intended, does a file it renamed or renumbered have every reference updated, and does an edit git merged cleanly still mean what it did on both sides? `git range-diff " + r.ReviewedBase + ".." + r.ReviewedHead + " $(git merge-base origin/main HEAD)..HEAD` shows it. Its output:\n\n```\n" + defang(rangeDiff) + "\n```" +
		"\n\nFix a real defect as a LOCAL commit, then emit your `## Review:` report with its Disposition line as usual. A disposition that blocks keeps the branch in its worktree for a human; nothing is pushed until this review clears."
}

// codeList renders paths as a comma-separated list of code spans, defanged.
func codeList(paths []string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = "`" + defang(p) + "`"
	}
	return strings.Join(quoted, ", ")
}

// ciFix builds the `-p` prompt for a sandboxed session that diagnoses and
// fixes a GitHub CI failure on the already-pushed PR (BEH-414). The PR is open
// and red; local gates passed but CI didn't (env/toolchain/flake/lockfile skew),
// so the harness fetched the failing job logs host-side and injects them here for
// the agent to reproduce + fix over the SAME existing worktree. As with review,
// the harness owns all remote I/O (ADR-0002): the agent commits the fix locally
// and stops — the harness re-pushes and re-polls CI. This prompt invokes no skill,
// so it is harness-composed; a loop's fix step may lead it with a body.
//
// ciFixLogSteer returns the log-framing block and the "your job" block for the
// fix prompt, branched on whether the harness actually fetched a usable failing
// log. When it could not (the step log expired, or the failure was an infra-level
// kill), that absence is the single strongest signal the failure is NOT a
// deterministic code defect — so the prompt warns the agent up front and gives it
// an explicit flake early-exit, rather than letting it assume a real, reproducible
// failure and exhaustively re-derive every PR gate to a dead end (BEH-558).
func ciFixLogSteer(branch, ciLogs string, logAvailable bool) (logFraming, job []string) {
	if logAvailable {
		return []string{
				"Here are the failing CI job logs the harness fetched for you (host-side, via `gh run view --log-failed`):",
				"",
				"```",
				ciLogs,
				"```",
			}, []string{
				"Your job: read those logs, reproduce/diagnose the failure locally in the worktree where you can, and apply a fix as a NEW LOCAL commit on `" + branch + "`. Re-run the relevant gate locally (the specific test/lint/typecheck/check that failed) to verify the fix before you stop. Keep the fix to its own commit — do NOT amend or force-push over the existing history.",
			}
	}
	return []string{
			"WARNING: the harness could NOT fetch the failing step's log (`gh run view --log-failed` returned no usable log — the log expired or the step was an infra-level kill). An unfetchable log strongly implies this is a FLAKE or infra kill, NOT a deterministic code defect — there is most likely nothing in the diff to fix. The harness captured only this, which is probably just an error marker:",
			"",
			"```",
			ciLogs,
			"```",
		}, []string{
			"Your job: identify the SINGLE gate CI reported as failing and reproduce JUST that one gate locally in the worktree. Do NOT exhaustively re-run every gate the PR runs (check, typecheck, the lint guards, the full unit suite, the browser-backed Storybook suite) — the unfetchable log already tells you this is most likely a flake, and re-deriving all of them only burns the budget to reach the same verdict.",
			"",
			"Early-exit rule: if that one failing gate is green locally — or you cannot even tell which gate failed because the log is gone — report `flake — no fix` and stop. Commit a fix ONLY if you actually reproduce a real, code-level failure; do not invent a fix for a phantom failure.",
		}
}

// browserTestSteer is how a CI-fix session runs a browser-backed test (#248). In
// the incident a session re-ran a hung Storybook story under `timeout 500`, then
// in the background with a poll loop, and the session cap killed it with nothing
// committed: the outer timeout killed pnpm but left the headless browser behind
// to block the next run, and the run never ended because the fix itself made the
// story hang. The reaper hook (internal/sandbox) clears the leftover; this steers
// off the moves that made one.
const browserTestSteer = "If the failing gate is a browser-backed test (a Storybook story file, a vitest browser run), run just the failing file, once, in the foreground: no outer `timeout` wrapper, no `&` or background run, no poll loop. An outer `timeout` kills the package manager but not its headless browser, and the browser left behind slows or blocks the next run; bound the call with the Bash tool's own timeout parameter (about 5 minutes) instead. The harness kills any headless browser or vitest process left over from an earlier run before each story run starts. Commit the candidate fix as a local commit before the run that verifies it, so the session's time cap cannot throw it away; if that run shows the fix is wrong, fix forward in a further commit, or `git revert` it if you find no fix. A verification run that makes no progress for a few minutes is a failed fix, not a slow run: the change made the test hang. Stop waiting on it, do not re-run it unchanged, and try another approach."

// mergeRefSteer is the CI-fix session's first diagnosis step (#258). CI on a
// pull_request event builds the PR's merge ref, not the branch: in the incident
// origin/main had already landed its own fix for the same base break, the
// session fixed it again on the branch alone, and the merge ref went red on the
// duplicate until a second session merged origin/main anyway.
const mergeRefSteer = "Before you diagnose, check origin/main. CI on a pull request tests the PR's merge ref — the branch merged into the current origin/main — not the branch alone, so a branch that is green locally can be red on CI because of what landed on origin/main since it was cut. The harness already fetched origin/main into this worktree before the session started; use that ref and do not try to fetch it again. If the failure does not reproduce on the branch alone, or the failing symbol or file was touched on origin/main since the merge-base (`git log HEAD..origin/main -- <path>`), reproduce it on a scratch merge: `git merge --no-commit --no-ff origin/main`, run the failing gate, then `git merge --abort` to return to the branch. When origin/main already fixes the failure, or conflicts with it (both sides now declare the same thing), resolve it by merging origin/main as a real commit (`git merge --no-ff origin/main`, resolving any conflict in favour of origin/main's version of the base fix), not with a parallel fix on the branch that duplicates what origin/main already did. Re-run the failing gate on that merge commit before you stop."

func ciFix(c Context) string {
	t, branch, worktreePath := c.Ticket, c.branch(), c.WorktreePath
	logFraming, job := ciFixLogSteer(branch, defang(c.CILogs), c.CILogAvailable)
	lines := []string{
		"A GitHub CI check is failing on the open PR for " + t.Identifier + ". The worktree already exists at `" + worktreePath + "` on branch `" + branch + "` — work in it; do NOT create a new worktree.",
		"",
		"The branch already passed the harness's local gate re-run, but CI on GitHub went red. The cause is usually a CI-vs-local difference (a different toolchain/env, a lockfile or native-binding skew, or a flaky spec) rather than something the local gate could catch — or merge-ref skew.",
		"",
		mergeRefSteer,
		"",
	}
	lines = append(lines, logFraming...)
	lines = append(lines, "")
	lines = append(lines, job...)
	lines = append(lines,
		"",
		"This sandbox has NO network and NO `gh` — the harness already fetched (or failed to fetch) those logs for you host-side. The logs above are all you get; do NOT try to run `gh`, `git fetch`, or any network command to fetch the CI logs, the run, or anything else yourself — it will only fail with `command not found` / no route to host and burn a turn. Work from the logs above plus local reproduction.",
		"",
		"Escape hatch — do NOT fabricate a commit. If you reproduce every failing gate locally and they all pass (the red is a cancelled or superseded run, not a reproducible code defect), then make NO commit at all and stop. Do NOT manufacture a speculative or unrelated change (a throwaway story, a no-op tweak, a base-refresh merge that fixes nothing you reproduced) just to give the loop something to push — that only pollutes the PR with unverified commits. When you leave the worktree clean with no new commit, the harness re-triggers CI itself with an empty commit, which clears a cancelled/superseded run. Only commit a fix for a failure you actually reproduced.",
		"",
		browserTestSteer,
		"",
		"Ticket context (the intent — already fetched for you):",
		"",
		"# "+t.Identifier+": "+defang(t.Title),
		"",
		defang(t.Description),
		"",
		"---",
		"",
		"Commit the fix LOCALLY and stop there. Do NOT push, do NOT run `gh`, do NOT open or comment on a PR. The harness owns all remote git I/O (ADR-0002): after you commit, it re-pushes the branch and re-polls CI itself.",
		"",
		trackerOff(c.Tracker),
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the local fix commit, or no commit at all when there is nothing real to fix.",
		"",
		bashQuirkSteer,
	)
	return withFixBody(c, withCheckpoint(c, lines))
}

// withFixBody leads a fix session's prompt with the Project body a loop's `fix`
// step supplies (ADR-0009), separated from the harness's own text. Without one the
// prompt is the envelope alone, as it always was: these sessions invoke no skill.
func withFixBody(c Context, lines []string) string {
	if strings.TrimSpace(c.Body) == "" {
		return strings.Join(lines, "\n")
	}
	return join(body(c), "---", strings.Join(lines, "\n"))
}

// leaveFullGateToHost keeps a conflict-resolution session from proving the
// resolved HEAD green with the project's full gate before it stops (#288). The
// harness re-runs every gate host-side on that same HEAD as soon as the session
// exits, so an in-sandbox run paid the gate twice in a row and told the host
// nothing it was not about to learn. The touched packages' build and tests stay:
// they are the fast check that the hand-merged hunks compile.
const leaveFullGateToHost = "After resolving, compile and run the tests of the packages you touched, and stop there. Do NOT run the project's full gate command: the harness runs it host-side on the resulting HEAD right after you stop and blocks the push if it is red, so running it here only pays the same gate twice."

// rebaseFix builds the `-p` prompt for the sandboxed conflict-resolution
// session the review stage launches when the proactive pre-push rebase hits a
// genuine content conflict (BEH-581). The branch already passed the cold review +
// the harness gate, but a sibling PR advanced origin/main underneath it and the
// two diffs genuinely overlap, so the automatic replay can't apply. Rather than
// dead-end and strand the reviewed work, the harness runs this session to rebase +
// resolve in the worktree — mirroring ciFix's local-commit-only contract. The
// harness then independently re-runs the gate host-side before pushing, so the
// session never pushes or touches the remote/tracker itself. This prompt invokes no
// skill, so it is harness-composed; a loop's fix step may lead it with a body
// (withFixBody).
func rebaseFix(c Context) string {
	if c.Merge {
		return mergeFix(c)
	}
	t, branch, worktreePath := c.Ticket, c.branch(), c.WorktreePath
	lines := []string{
		"A pre-push rebase for " + t.Identifier + " hit a genuine content conflict. The branch already passed the cold review and the harness gate, but origin/main advanced underneath it (a sibling PR merged) and the changes overlap, so it cannot be replayed automatically. The worktree already exists at `" + worktreePath + "` on branch `" + branch + "` — work in it; do NOT create a new worktree.",
		"",
		"Do NOT use `git rebase` here. This is a LINKED worktree (`git worktree add`), and `git rebase`'s detach-to-onto checkout false-fails with `Your local changes to the following files would be overwritten by merge` / `could not detach HEAD` even when the tree is byte-clean (`git status` empty) — a worktree checkout-safety artifact, not a real conflict. Burning attempts on `git rebase`/`git rebase --merge` here is wasted effort (BEH-618).",
		"",
		"Your job: replay this branch onto origin/main via `reset --hard` + `cherry-pick`, which does NOT hit that false-fail. origin/main is already fetched locally (this sandbox has NO network), so it all works offline. In the worktree run, in order:",
		"  1. `git branch _premerge HEAD` — mark the current feature tip so the replay range can name it.",
		"  2. `git reset --hard origin/main` — move the branch onto the fresh base (this is the full-tree move `git rebase` botches in a linked worktree, but `reset --hard` does cleanly).",
		"  3. `git cherry-pick origin/main.._premerge` — replay your feature commits onto the new base. cherry-pick's three-way merge surfaces only the genuine conflicts.",
		"  4. For each conflict: resolve it by hand, `git add` the resolved files, then `git cherry-pick --continue`. Repeat until the cherry-pick finishes and `git status` is clean.",
		"  5. `git branch -D _premerge` to drop the marker.",
		"",
		"If `git cherry-pick` (or `--continue`) reports `The previous cherry-pick is now empty` / `the previous cherry-pick is now empty`, that is NOT a conflict to resolve — that feature commit's change is already present identically in origin/main (a sibling PR merged the same change, or it was cherry-picked to main). Run `git cherry-pick --skip` to drop the now-redundant commit and continue the replay. Do NOT `git add` an empty tree or try to force an empty commit; just `--skip` it (BEH-622).",
		"",
		"If any step aborts with `untracked working tree files would be overwritten` pointing at `.worktree-ready`, that is NOT a content conflict — it is the gitignored readiness sentinel new-worktree.sh drops in every worktree. Remove it with `rm -f .worktree-ready` and re-run the step.",
		"",
		"Resolve conflicts to preserve the intent of BOTH sides: keep this ticket's change AND the incoming change from main. Do NOT blindly take one side (`--ours`/`--theirs`) — read both hunks and merge them so neither feature is lost. When in doubt, the ticket's intent is below.",
		"",
		"Do NOT run `git cherry-pick --abort` (or `git rebase --abort`) or otherwise give up — aborting would strand the branch on its stale base, which is exactly the dead-end this session exists to fix. If a hunk is genuinely impossible to reconcile, make your best-effort merge and leave a clear note in the commit; the harness re-runs the full gate host-side and watches CI, so a mistake surfaces there rather than silently shipping.",
		"",
		leaveFullGateToHost,
		"",
		"Ticket context (the intent to preserve — already fetched for you):",
		"",
		"# " + t.Identifier + ": " + defang(t.Title),
		"",
		defang(t.Description),
		"",
		"---",
		"",
		"Leave the resolved rebase committed in the worktree and stop there. Do NOT push, do NOT run `gh`, do NOT open or comment on a PR. The harness owns all remote git I/O (ADR-0002): after you finish the rebase, it independently re-runs the quality gates host-side and, only if they pass, pushes the rebased branch and opens the PR itself.",
		"",
		trackerOff(c.Tracker),
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the resolved, rebased worktree.",
		"",
		bashQuirkSteer,
	}
	return withFixBody(c, withCheckpoint(c, lines))
}

// mergeFix is the RebaseFix variant for a branch a human opted in to the PR loop
// (ADR-0019): its history must survive, because a force-push would rewrite commits
// someone else may have checked out. So the session merges origin/main INTO the
// branch and commits the merge, rather than replaying the branch onto it. The
// ground truth is the same as the rebase's — a clean tree with origin/main an
// ancestor of the tip — and the harness pushes the result without force.
func mergeFix(c Context) string {
	t, branch, worktreePath := c.Ticket, c.branch(), c.WorktreePath
	lines := []string{
		"The open PR " + t.Identifier + " conflicts with `main`: origin/main advanced underneath it and the changes overlap. The worktree already exists at `" + worktreePath + "` on branch `" + branch + "` — work in it; do NOT create a new worktree.",
		"",
		"A human owns this branch, so its history must NOT be rewritten. Do NOT rebase, `reset --hard`, cherry-pick onto a new base, amend, or squash. The harness pushes your result WITHOUT force, so any rewrite will be rejected.",
		"",
		"Your job: merge origin/main into this branch. origin/main is already fetched locally (this sandbox has NO network), so it all works offline. In the worktree run, in order:",
		"  1. `git merge --no-ff origin/main`.",
		"  2. For each conflict: resolve it by hand and `git add` the resolved files.",
		"  3. `git commit --no-edit` to record the merge. `git status` must be clean when you stop.",
		"",
		"Resolve conflicts to preserve the intent of BOTH sides: keep this PR's change AND the incoming change from main. Do NOT blindly take one side (`--ours`/`--theirs`) — read both hunks and merge them so neither feature is lost. When in doubt, the PR's intent is below.",
		"",
		"Do NOT run `git merge --abort` or otherwise give up — aborting leaves the PR conflicting, which is exactly the dead-end this session exists to fix. If a hunk is genuinely impossible to reconcile, make your best-effort merge and say so in the merge commit message; the harness re-runs the full gate host-side and watches CI, so a mistake surfaces there rather than silently shipping.",
		"",
		leaveFullGateToHost,
		"",
		"PR context (the intent to preserve — already fetched for you):",
		"",
		"# " + t.Identifier + ": " + defang(t.Title),
		"",
		defang(t.Description),
		"",
		"---",
		"",
		"Leave the merge committed in the worktree and stop there. Do NOT push, do NOT run `gh`, do NOT open or comment on a PR. The harness owns all remote git I/O (ADR-0002): after you finish, it independently re-runs the quality gates host-side and, only if they pass, pushes the branch itself.",
		"",
		trackerOff(c.Tracker),
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the merged worktree.",
		"",
		bashQuirkSteer,
	}
	return withFixBody(c, withCheckpoint(c, lines))
}
