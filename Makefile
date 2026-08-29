# Agent harness — Go toolchain shortcuts. Requires Go 1.26+.
.PHONY: build base-image test implementation review retrospective pipeline loop watch vet fmt fmt-check check-exec check-bash3 check-buildvcs check-ci check-scripts check tidy

# Build the tool binaries into ./bin.
#
# -buildvcs=false disables VCS stamping, which shells out to git and returns 128
# inside a linked `.claude/worktrees/*` worktree, aborting the build before it
# runs (BEH-459). It is a no-op in a normal checkout (CI), so it only unblocks
# worktree work. The check-buildvcs guard fails the gate if a VCS-stamping go
# command (build/install/run/test) ever drops the flag.
build:
	go build -buildvcs=false -o bin/implementation ./cmd/implementation
	go build -buildvcs=false -o bin/review ./cmd/review
	go build -buildvcs=false -o bin/retrospective ./cmd/retrospective
	go build -buildvcs=false -o bin/pipeline ./cmd/pipeline
	go build -buildvcs=false -o bin/loop ./cmd/loop
	go build -buildvcs=false -o bin/watch ./cmd/watch

# Build the sandbox BASE image — the harness<->sandbox contract every Consumer
# FROMs (ADR-0008), and what the release workflow publishes to GHCR. It carries no
# project toolchain, so it is not a runnable session image on its own: a Consumer
# builds ITS image from its own Dockerfile, and the harness does that for it on a
# local image miss. Override the tag with BASE_IMAGE=...
BASE_IMAGE ?= agent-harness-base:dev
base-image:
	docker build -f Dockerfile.base -t $(BASE_IMAGE) .


# Run the full test suite. -buildvcs=false: see the build target (BEH-459).
test:
	go test -buildvcs=false ./...

# Fetch + claim a ticket and run the sandboxed /tdd session. Pass the ticket id
# (and optional flags) in ARGS, e.g.  make implementation ARGS="BEH-362 --verbose"
implementation:
	go run -buildvcs=false ./cmd/implementation $(ARGS)

# Run a cold /review-worktree over the implementation worktree, then re-run the
# gates host-side and (if green) push + open the PR. Pass the ticket id (and
# optional flags) in ARGS, e.g.  make review ARGS="BEH-371 --verbose"
review:
	go run -buildvcs=false ./cmd/review $(ARGS)

# Run the sandboxed /retrospective session over a ticket's transcripts and file
# any findings. Pass the ticket id (and optional flags) in ARGS, e.g.
#   make retrospective ARGS="BEH-362 --verbose"
retrospective:
	go run -buildvcs=false ./cmd/retrospective $(ARGS)

# Run the single-ticket pipeline — implementation → review → retrospective — over
# one hand-passed ticket, then exit. Pass the ticket id (and optional flags) in
# ARGS, e.g.  make pipeline ARGS="BEH-362 --verbose"  or  ARGS="BEH-362 --dry-run"
pipeline:
	go run -buildvcs=false ./cmd/pipeline $(ARGS)

# Run the autonomous daemon: it drives the single-ticket pipeline over the
# ready-for-agent queue in a long-running loop, idling and re-polling when the
# queue is empty. Takes no required args — Ctrl-C stops it gracefully after the
# in-flight ticket; a second Ctrl-C hard-aborts and kills the running container.
loop:
	go run -buildvcs=false ./cmd/loop $(ARGS)

# Tail the global structured event stream (logs/loop.jsonl) and print live
# plain-text lines showing the current ticket + stage (ADR-0005). Read-only — it
# never controls the loop; Ctrl-C quits the viewer without touching the daemon. It
# works against `make loop` AND a single-shot `make pipeline`, since both write the
# stream. Pass a path in ARGS to tail a specific file.
watch:
	go run -buildvcs=false ./cmd/watch $(ARGS)

vet:
	go vet ./...

fmt:
	gofmt -w .

# Fail if anything is unformatted (the CI gate).
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needs to run on:"; echo "$$unformatted"; exit 1; \
	fi

# Check for unbounded exec.Command calls to daemon/remote tools (BEH-388).
check-exec:
	@./scripts/check-exec-command.sh

# Fail on bash-4-only syntax in bash scripts — they must run under macOS's stock
# bash 3.2 or they silently no-op there (BEH-457).
check-bash3:
	@./scripts/check-bash3-compat.sh

# Fail if a VCS-stamping go command in a Makefile drops -buildvcs=false, which
# breaks builds from a linked worktree (BEH-459).
check-buildvcs:
	@./scripts/check-buildvcs.sh

# Fail if the CI workflow re-enumerates check's sub-targets instead of running
# `make check` directly. Keeps a guard added to `check:` above from staying
# CI-invisible (the BEH-457/BEH-409 divergence). It's a prerequisite of `check`,
# so running `make check` in CI runs it for free (BEH-462).
check-ci:
	@./scripts/check-ci-runs-check.sh

# Run the agent-harness shell tests (scripts/test-*.sh) so a behavioral
# regression in a load-bearing harness script (loop-start.sh, the check-* guards)
# fails the gate instead of shipping green — the tests passed by hand but nothing
# ran them (BEH-591). Diff-based coverage enforcement (a changed script whose
# sibling test-<name>.sh isn't touched) lives as a separate CI step in
# agent-harness.yaml, mirroring the repo-root split, so `make check` stays
# offline-runnable.
check-scripts:
	@./scripts/run-script-tests.sh

# The pre-push gate: format check, vet, the script guards, and the full test suite.
check: fmt-check vet check-exec check-bash3 check-buildvcs check-ci check-scripts test

tidy:
	go mod tidy
