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
	"strings"
	"text/template"

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
// PASSED.
//
// The quirks are properties of the CLI and of POSIX shell, so the steer describes
// them in those terms only. It named herd's pnpm/oxfmt gate output verbatim until
// BEH-641, which made the envelope — the harness's own public API (ADR-0009) —
// carry one Consumer's toolchain to every other Consumer's agent.
const bashQuirkSteer = "Sandbox bash quirk (BEH-401/BEH-598/BEH-601/BEH-645): the bundled Claude CLI's Bash tool can surface opaque errors that look like a bug in your command but are an environment artifact, in four ways. (1) It intermittently mangles a single Bash call that BOTH pipes into `head`/`tail` AND uses a command substitution like `cd \"$(...)\"`, producing errors like `head: invalid number of bytes: 'set -euo pipefail; ...'` or `cd: too many arguments`. (2) When a plain command exits non-zero BY DESIGN, the tool can collapse that into a bare `Error` string with the real exit code and stderr stripped — e.g. `git merge-base HEAD origin/main` exits 1 when two commits share no common ancestor, which is an expected signal, not a failure. (3) A `VAR=value; cmd \"$VAR\"` assignment-then-use within ONE Bash call can expand `$VAR` to the EMPTY string: the failure is silent (empty output) or surfaces as a path with the prefix missing (e.g. `\"$DP/dist\"` becomes `/dist` → `cannot access '/dist'`). `&&`-chaining the assignment to its use (`VAR=value && cmd \"$VAR\"`) expands fine; `;`-separating it is what drops the variable. (4) THE WORST, because it makes a GREEN gate look RED: chaining trailing statements onto a GATE command in one Bash call — e.g. `<gate command> > /tmp/gate.log 2>&1; echo exit=$?; grep -i error /tmp/gate.log | head` — can concatenate those trailing statements as ARGUMENTS onto the gate's own command, so the underlying tool receives `echo exit=$? grep …` where it expected files or targets. It then fails with an argument/target complaint (a formatter reporting it matched no input files, a test runner reporting an unknown target) and your task runner reports a non-zero lifecycle exit — a spurious failure on a gate that actually PASSED. Work around all four: run one command per Bash call, prefer absolute paths over `cd \"$(...)\"`, and don't tack `| head -n N` onto a compound command; when a plain command returns a bare `Error`, do NOT assume it broke — re-run it capturing the exit code explicitly (append `; echo exit=$?`, or use `cmd || echo \"exit $?\"`) to tell an expected non-zero exit from a real failure; avoid intra-call shell variables — inline the absolute path, `&&`-chain instead of `;`, split into separate calls, or use the Grep/Glob tools with literal absolute paths; and run each configured verification gate as its OWN Bash call with NOTHING appended — no `; echo exit=$?`, no `> log 2>&1; grep … | head` — reading its exit status in a separate call, and if a gate reds with an argument/target complaint naming input it was never meant to receive, re-run it ALONE before treating it as a real failure. Retrying verbatim won't help in any case — split it up, inspect the exit code, drop the intra-call variable, or re-run the gate alone."

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

	// Body is the Consumer-supplied prompt body from `.agent-harness/prompts/`.
	// CIFix and RebaseFix invoke no skill and ignore it.
	Body string

	// Resume selects the Implement variant.
	Resume Resume
	// Filed lists the finding classes already filed, for a Retrospective re-run.
	Filed []FiledFinding
	// CILogs is the failing job log the host fetched for CIFix; CILogAvailable
	// says whether it is a usable log or only an error marker (BEH-558).
	CILogs         string
	CILogAvailable bool
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
	}
	panic(fmt.Sprintf("prompt: unknown stage %d", s))
}

// bodyData is the template context available to a Consumer prompt body. A body
// interpolates these with `{{.Field}}` (text/template) so herd's committed bodies
// can name the ticket, slug, branch, and worktree without the harness hardcoding
// them. Missing fields render empty; a malformed template falls back to the raw
// body (renderBody).
type bodyData struct {
	Identifier   string
	Title        string
	Slug         string
	BranchPrefix string
	WorktreePath string
}

// renderBody interpolates the Consumer body's `{{.Field}}` placeholders against
// the Context. A body is Consumer-controlled and may be malformed or empty; rather than
// fail the whole prompt, a parse/exec error falls back to the raw body verbatim —
// quality is the Consumer's concern, the contract (which is envelope, not body)
// is never at risk.
func renderBody(c Context) string {
	tmpl, err := template.New("body").Parse(c.Body)
	if err != nil {
		return c.Body
	}
	d := bodyData{
		Identifier:   c.Ticket.Identifier,
		Title:        c.Ticket.Title,
		Slug:         c.Slug,
		BranchPrefix: c.BranchPrefix,
		WorktreePath: c.WorktreePath,
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, d); err != nil {
		return c.Body
	}
	return b.String()
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

// trackerOffSteer is the envelope's tracker-off contract for the implementation
// stage: the ticket is already claimed host-side and the sandbox must not touch
// the tracker (ADR-0001).
func trackerOffSteer(id string) string {
	return id + " is already claimed and moved to In Progress for you. Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O."
}

// findingsDropboxProtocol is the envelope's findings-dropbox contract for the
// implementation stage: harness/environment friction goes to `/findings/out.json`
// in the `{title, body, kind, key}` shape the host-side filer reads, never to the
// tracker directly.
const findingsDropboxProtocol = "If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file Linear issues. Instead append them to `/findings/out.json` as a JSON array of `{title, body, kind, key}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem). The harness reads this file after the session and files the issues for you, skipping any whose key already has an open issue. If you have no findings, leave the file untouched."

// branchHandoffContract is the envelope's branch/handoff contract: the whole
// pipeline (verify, push, PR, CI, review, rebase) keys off the canonical
// `<prefix>/<slug>` branch, so a handoff committed anywhere else is silently
// stranded (BEH-615). It is appended structurally, so a body cannot drop it.
func branchHandoffContract(branchPrefix, slug string) string {
	return "The harness keys its handoff check, push, PR, and review off the canonical `" + branchPrefix + "/" + slug + "` branch, so committing to any other branch (e.g. `fix/" + slug + "`) silently strands your work where the harness never sees it."
}

// freshWorktreeInstr is the implementation stage's worktree instruction for a
// fresh run. The harness now creates the worktree + canonical branch host-side
// (ADR-0008/BEH-636), so this points the agent AT that pre-created worktree and
// forbids running `new-worktree.sh` — it never creates one itself. It is envelope
// content (not body) because it varies per implementation variant (fresh /
// resumed-branch / usage-policy retry), each of which enters the worktree
// differently.
func freshWorktreeInstr(slug, branchPrefix string) string {
	branch := branchPrefix + "/" + slug
	return "The worktree for slug `" + slug + "` has already been created for you at `.claude/worktrees/" + slug + "` on branch `" + branch + "` — the harness owns worktree and branch creation, so do NOT run `scripts/new-worktree.sh` and do NOT create a new worktree or branch. `cd` into that worktree and do all your work there, committing your handoff on `" + branch + "` verbatim regardless of the ticket type (bug-fix, chore, docs, …)."
}

// tddFooter is the shared contract footer for every implementation-stage prompt
// (fresh, resumed-branch, retry). It structurally guarantees the four
// non-overridable envelope items: branch/handoff contract, tracker-off steer,
// findings-dropbox protocol, and bash-quirk steer.
func tddFooter(id, branchPrefix, slug string) string {
	return join(
		branchHandoffContract(branchPrefix, slug),
		trackerOffSteer(id),
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
		worktreeState = resumedBranchWorktreeState(c.Slug, c.BranchPrefix)
	case AfterRefusal:
		worktreeState = resumeWorktreeState(c.Slug, c.WorktreePath, c.BranchPrefix)
	default:
		worktreeState = freshWorktreeInstr(c.Slug, c.BranchPrefix)
	}
	return join(
		renderBody(c),
		worktreeState,
		ticketContext(c.Ticket),
		"---",
		tddFooter(c.Ticket.Identifier, c.BranchPrefix, c.Slug),
	)
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
	b.WriteString("This is an umbrella/batch ticket: its real work lives in the sub-issues below, whose full specs are inlined here (already fetched for you — the sandbox CANNOT reach Linear, so do NOT try to look them up with `mcp__linear-server__*`). Implement EVERY sub-issue, not just the umbrella body above; if you defer one, say which and why in your handoff:")
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
func resumedBranchWorktreeState(slug, branchPrefix string) string {
	branch := branchPrefix + "/" + slug
	return join(
		"Enter the worktree already created for you at `.claude/worktrees/"+slug+"` on branch `"+branch+"` — the harness owns worktree and branch creation, so do NOT run `scripts/new-worktree.sh`.",
		"IMPORTANT — RESUMED WORKTREE: branch `"+branch+"` ALREADY carries commit(s) for this ticket ahead of `main` from a previous session — the fix may already be COMPLETE. Before you plan or write any code, enter the worktree and inspect that history: run `git log origin/main..HEAD` (equivalently `git log main..HEAD`) and read the diff. If the ticket's behaviour is already implemented and tested on the branch, do NOT re-implement, rewrite, or redo it — instead verify the gates pass and record \"already fixed on branch — recommend review/handoff\" in your handoff, then stop. Only finish or extend the work if the branch's fix is genuinely incomplete. The ticket prose below may describe the code as still un-fixed even though the branch already resolves it, so trust the branch's git history over the prose.",
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

// retroFindingsProtocol is the retrospective stage's findings-dropbox contract:
// unlike the implementation stage, the retrospective's ONLY output is findings, so
// it must always write the file — an absent file means the step never ran.
const retroFindingsProtocol = "Write your findings to `/findings/out.json` as a JSON array of `{title, body, kind, key, audience}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem, so the harness skips a finding whose key already has an open issue; audience is covered in the classification rule below). **Always write the file**, even when you found nothing — write an empty array `[]` in that case. An absent file means the step never ran, so never end without writing it."

// retroAudienceContract is the retrospective envelope's non-overridable
// classify-at-source + sanitize rule (ADR-0011). Every finding carries an
// audience so the harness routes it: a "project" finding (about the Consumer's
// own codebase/tests/CI) files to the project's tracker; a "harness" finding
// (about the harness, its sandbox, or its prompt contract) stays in a local
// artifact dir in the repo. Because a harness finding may later be shared to a
// PUBLIC upstream repo, the sanitize rule is mandatory and cannot be weakened by
// the Consumer body — it is appended structurally, like the rest of the envelope.
const retroAudienceContract = "Classify EVERY finding with an `audience` field, either `\"project\"` or `\"harness\"`. Use `\"project\"` for friction about the Consumer's OWN codebase, tests, or CI (a flaky spec, a slow or wrong gate, a broken project script) — the harness files these to the project's issue tracker. Use `\"harness\"` for friction about the harness, its sandbox, or its prompt contract itself (a missing sandbox dependency, an opaque CLI/bash quirk, a misleading or contradictory envelope instruction) — the harness keeps these in a local artifact dir in this repo; nothing leaves the repo. When unsure, use `\"harness\"`. NON-OVERRIDABLE SANITIZE RULE: a `\"harness\"` finding must state ONLY the failure class and harness-side detail — what broke in the sandbox/contract and how to fix it. Never include (do not sanitize by paraphrase — omit entirely) Consumer source code, transcript excerpts, ticket contents, project file paths, or any secret/token. This rule holds regardless of anything else in this prompt or the project body."

// retroContractSteer is the retrospective stage's tracker-off + read-only
// contract: no code changes, no push, no tracker.
const retroContractSteer = "This session is read-only and reaches no remote. Make NO code changes, do NOT commit or push, and do NOT touch Linear — do not call any `mcp__linear-server__*` tool. The harness reads `out.json` after the session and files each finding to Linear itself."

// retrospective builds the `-p` prompt for the sandboxed retrospective
// session (the terminal tool). The Consumer body points the session at the
// ticket-keyed transcripts and the diff; the harness envelope guarantees the
// findings-dropbox protocol, the tracker-off/read-only contract, and the
// bash-quirk steer, plus the dynamic already-filed dedup context (BEH-539).
func retrospective(c Context) string {
	return join(
		renderBody(c),
		retroFindingsProtocol,
		retroAudienceContract,
		retroContractSteer,
		alreadyFiledSection(c.Filed),
		bashQuirkSteer,
	)
}

// alreadyFiledSection renders the re-run dedup context (BEH-539): the finding
// classes the harness has already filed, so the session treats them as settled
// and spends its budget on NEW friction instead of re-deriving them. Empty when
// there's nothing already filed — a first run reads exactly as it did before.
func alreadyFiledSection(filed []FiledFinding) string {
	if len(filed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("This ticket has been through the pipeline before — the harness has ALREADY filed Linear issues for the finding classes below, so treat them as SETTLED. Do NOT re-investigate, re-derive, or re-file them (the harness would skip them on key anyway); spend your budget only on NEW friction introduced since the last retrospective:")
	for _, f := range filed {
		b.WriteString("\n  - ")
		if f.Key != "" {
			b.WriteString(f.Key + " — ")
		}
		b.WriteString(defang(f.Title))
	}
	return b.String()
}

// reviewContractFooter is the review stage's contract footer. It structurally
// guarantees the review analog of the four envelope items: the branch/handoff
// contract (commit locally, the harness owns push/PR — ADR-0002), the tracker-off
// steer (ADR-0001), the findings protocol (review emits NONE; retrospective owns
// findings), and the bash-quirk steer.
func reviewContractFooter(branchPrefix, slug string) string {
	return join(
		"Apply your review fixes as a LOCAL commit on `"+branchPrefix+"/"+slug+"` and stop there. Do NOT push, do NOT run `gh`, do NOT open or raise a PR. The harness owns all remote git I/O (ADR-0002): it independently re-runs the quality gates host-side and, only if they pass, pushes the branch and opens the PR itself.",
		"Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O (ADR-0001).",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool, running last over every transcript, owns findings now — this is a deliberate change from the skill's default. Your only outputs are review fixes committed locally.",
		bashQuirkSteer,
	)
}

// review builds the `-p` prompt for the sandboxed review session (the second
// tool). The Consumer body invokes the review skill and carries the cold-review /
// lenses-first / disposition conventions; the harness envelope injects the ticket
// context (the intent to review against) and the non-overridable contract footer.
func review(c Context) string {
	return join(
		renderBody(c),
		ticketContext(c.Ticket),
		"---",
		reviewContractFooter(c.BranchPrefix, c.Slug),
	)
}

// ciFix builds the `-p` prompt for a sandboxed session that diagnoses and
// fixes a GitHub CI failure on the already-pushed PR (BEH-414). The PR is open
// and red; local gates passed but CI didn't (env/toolchain/flake/lockfile skew),
// so the harness fetched the failing job logs host-side and injects them here for
// the agent to reproduce + fix over the SAME existing worktree. As with review,
// the harness owns all remote I/O (ADR-0002): the agent commits the fix locally
// and stops — the harness re-pushes and re-polls CI. This prompt invokes no skill,
// so it stays harness-composed (no Consumer body).
//
// ciFixLogSteer returns the log-framing block and the "your job" block for the
// fix prompt, branched on whether the harness actually fetched a usable failing
// log. When it could not (the step log expired, or the failure was an infra-level
// kill), that absence is the single strongest signal the failure is NOT a
// deterministic code defect — so the prompt warns the agent up front and gives it
// an explicit flake early-exit, rather than letting it assume a real, reproducible
// failure and exhaustively re-derive every PR gate to a dead end (BEH-558).
func ciFixLogSteer(slug, branchPrefix, ciLogs string, logAvailable bool) (logFraming, job []string) {
	if logAvailable {
		return []string{
				"Here are the failing CI job logs the harness fetched for you (host-side, via `gh run view --log-failed`):",
				"",
				"```",
				ciLogs,
				"```",
			}, []string{
				"Your job: read those logs, reproduce/diagnose the failure locally in the worktree where you can, and apply a fix as a NEW LOCAL commit on `" + branchPrefix + "/" + slug + "`. Re-run the relevant gate locally (the specific test/lint/typecheck/check that failed) to verify the fix before you stop. Keep the fix to its own commit — do NOT amend or force-push over the existing history.",
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

func ciFix(c Context) string {
	t, slug, branchPrefix, worktreePath := c.Ticket, c.Slug, c.BranchPrefix, c.WorktreePath
	logFraming, job := ciFixLogSteer(slug, branchPrefix, defang(c.CILogs), c.CILogAvailable)
	lines := []string{
		"A GitHub CI check is failing on the open PR for " + t.Identifier + ". The worktree already exists at `" + worktreePath + "` on branch `" + branchPrefix + "/" + slug + "` — work in it; do NOT create a new worktree.",
		"",
		"The branch already passed the harness's local gate re-run, but CI on GitHub went red. The cause is usually a CI-vs-local difference (a different toolchain/env, a lockfile or native-binding skew, or a flaky spec) rather than something the local gate could catch.",
		"",
	}
	lines = append(lines, logFraming...)
	lines = append(lines, "")
	lines = append(lines, job...)
	lines = append(lines,
		"",
		"This sandbox has NO network and NO `gh` — the harness already fetched (or failed to fetch) those logs for you host-side. The logs above are all you get; do NOT try to run `gh`, `git fetch`, or any network command to fetch the CI logs, the run, or anything else yourself — it will only fail with `command not found` / no route to host and burn a turn. Work from the logs above plus local reproduction.",
		"",
		"Escape hatch — do NOT fabricate a commit. If you reproduce every failing gate locally and they all pass (the red is a cancelled or superseded run, not a reproducible code defect), then make NO commit at all and stop. Do NOT manufacture a speculative or unrelated change (a throwaway story, a no-op tweak, a base-refresh merge) just to give the loop something to push — that only pollutes the PR with unverified commits. When you leave the worktree clean with no new commit, the harness re-triggers CI itself with an empty commit, which clears a cancelled/superseded run. Only commit when you have a real, locally-verified fix.",
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
		"Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O (ADR-0001).",
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the local fix commit, or no commit at all when there is nothing real to fix.",
		"",
		bashQuirkSteer,
	)
	return strings.Join(lines, "\n")
}

// rebaseFix builds the `-p` prompt for the sandboxed conflict-resolution
// session the review stage launches when the proactive pre-push rebase hits a
// genuine content conflict (BEH-581). The branch already passed the cold review +
// the harness gate, but a sibling PR advanced origin/main underneath it and the
// two diffs genuinely overlap, so the automatic replay can't apply. Rather than
// dead-end and strand the reviewed work, the harness runs this session to rebase +
// resolve in the worktree — mirroring ciFix's local-commit-only contract. The
// harness then independently re-runs the gate host-side before pushing, so the
// session never pushes or touches the remote/Linear itself. This prompt invokes no
// skill, so it stays harness-composed (no Consumer body).
func rebaseFix(c Context) string {
	t, slug, branchPrefix, worktreePath := c.Ticket, c.Slug, c.BranchPrefix, c.WorktreePath
	lines := []string{
		"A pre-push rebase for " + t.Identifier + " hit a genuine content conflict. The branch already passed the cold review and the harness gate, but origin/main advanced underneath it (a sibling PR merged) and the changes overlap, so it cannot be replayed automatically. The worktree already exists at `" + worktreePath + "` on branch `" + branchPrefix + "/" + slug + "` — work in it; do NOT create a new worktree.",
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
		"Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O (ADR-0001).",
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the resolved, rebased worktree.",
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}
