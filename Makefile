# Agent harness — Go toolchain shortcuts. Requires Go 1.26+.
.PHONY: build image test implementation review retrospective vet fmt fmt-check check-exec check tidy

# Build the tool binaries into ./bin.
build:
	go build -o bin/implementation ./cmd/implementation
	go build -o bin/review ./cmd/review
	go build -o bin/retrospective ./cmd/retrospective

# Build the sandbox image the tools launch. Override the tag with IMAGE=...
# (must match HARNESS_IMAGE if you set it).
IMAGE ?= herd-agent-harness:latest
image:
	docker build -t $(IMAGE) .

# Run the full test suite.
test:
	go test ./...

# Fetch + claim a ticket and run the sandboxed /tdd session. Pass the ticket id
# (and optional flags) in ARGS, e.g.  make implementation ARGS="BEH-362 --verbose"
implementation:
	go run ./cmd/implementation $(ARGS)

# Run a cold /review-worktree over the implementation worktree, then re-run the
# gates host-side and (if green) push + open the PR. Pass the ticket id (and
# optional flags) in ARGS, e.g.  make review ARGS="BEH-371 --verbose"
review:
	go run ./cmd/review $(ARGS)

# Run the sandboxed /retrospective session over a ticket's transcripts and file
# any findings. Pass the ticket id (and optional flags) in ARGS, e.g.
#   make retrospective ARGS="BEH-362 --verbose"
retrospective:
	go run ./cmd/retrospective $(ARGS)

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

# The pre-push gate: format check, vet, exec guard, and the full test suite.
check: fmt-check vet check-exec test

tidy:
	go mod tidy
