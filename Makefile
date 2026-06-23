# Agent harness — Go toolchain shortcuts. Requires Go 1.26+.
.PHONY: build image smoke test implementation review retrospective pipeline vet fmt fmt-check check-exec check-bash3 check-buildvcs check-ci check tidy

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

# Build the sandbox image the tools launch. Override the tag with IMAGE=...
# (must match HARNESS_IMAGE if you set it).
IMAGE ?= herd-agent-harness:latest
image:
	docker build -t $(IMAGE) .

# Behavioral smoke check: run pnpm inside the built image and assert it resolves
# its store under the mounted pnpm-store volume (BEH-487). Unlike the
# Dockerfile-text invariant in `make test`, this measures the runtime effect, so
# it catches a `store-dir` line that is present but wired with a prefix the
# image's pnpm doesn't honour. Needs Docker; HARNESS_DOCKER_SMOKE=1 opts the
# otherwise-skipped test in (and it builds the image on miss). Override the tag
# with IMAGE=... (exported as HARNESS_IMAGE so the test sees it).
smoke:
	HARNESS_DOCKER_SMOKE=1 HARNESS_IMAGE=$(IMAGE) go test -buildvcs=false -count=1 -run TestPnpmStoreResolvesUnderMountedVolumeInImage ./internal/sandbox

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

# The pre-push gate: format check, vet, the script guards, and the full test suite.
check: fmt-check vet check-exec check-bash3 check-buildvcs check-ci test

tidy:
	go mod tidy
