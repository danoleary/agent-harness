// Command run-tdd is the Phase-1 harness CLI: fetch + claim one hand-passed
// ticket, run only the /tdd session in a Docker sandbox, verify the worktree +
// handoff commit by ground truth, and file any dropped findings. No selection,
// no review, no PR, no loop (those are later phases). See docs/DESIGN.md.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/findings"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/prompt"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/stream"
	"github.com/beherd/agent-harness/internal/verify"
)

var ticketRE = regexp.MustCompile(`^[A-Z]+-\d+$`)

type cliArgs struct {
	identifier string
	dryRun     bool
	verbose    bool
}

func parseArgs(argv []string) (cliArgs, error) {
	var a cliArgs
	for _, arg := range argv {
		switch {
		case arg == "--dry-run":
			a.dryRun = true
		case arg == "--verbose":
			a.verbose = true
		case !strings.HasPrefix(arg, "-") && a.identifier == "":
			a.identifier = strings.ToUpper(arg)
		}
	}
	if !ticketRE.MatchString(a.identifier) {
		return a, fmt.Errorf("usage: run-tdd <TICKET-ID> [--dry-run] [--verbose]  (got: %q)", a.identifier)
	}
	return a, nil
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run() (int, error) {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		return 1, err
	}

	loadDotEnv(".env")
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return 1, err
	}

	runID := runlog.MakeRunID(time.Now())
	log, err := runlog.New(filepath.Join(cfg.HerdPath, "agent-harness", "logs"), runID)
	if err != nil {
		return 1, err
	}
	slug := strings.ToLower(args.identifier)

	dry := ""
	if args.dryRun {
		dry = " (dry-run)"
	}
	log.Event(fmt.Sprintf("run %s — tdd %s%s", runID, args.identifier, dry))

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	t, err := client.FetchTicket(args.identifier)
	if err != nil {
		return 1, err
	}
	priority := t.Priority
	if priority == "" {
		priority = "No priority"
	}
	log.Event(fmt.Sprintf("fetched %s (%s) — %s", t.Identifier, priority, t.Title))

	p := prompt.BuildTdd(t, slug)
	findingsDir := filepath.Join(log.RunDir, "findings", args.identifier+"-tdd")
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		return 1, err
	}

	// runID is second-resolution; include the pid so two runs started in the same
	// second still get distinct container names (and distinct `docker kill` targets).
	containerName := fmt.Sprintf("herd-harness-%s-%d-tdd", runID, os.Getpid())
	dockerArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     findingsDir,
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          p,
		Model:           cfg.Model,
		ContainerName:   containerName,
	})

	if args.dryRun {
		log.Event("dry-run — not claiming the ticket, not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(dockerArgs, " "),
		)
		return 0, nil
	}

	// Fail fast if Docker can't run the container, so we never claim a ticket we
	// cannot actually work (the launch failure would otherwise leave it In
	// Progress with no worktree — BEH-316's exit-125 footgun).
	if err := sandbox.Preflight(cfg.Image, execCombinedOutput); err != nil {
		return 1, err
	}

	if err := client.MoveToInProgress(args.identifier); err != nil {
		return 1, err
	}
	log.Event(fmt.Sprintf("claimed %s → In Progress", args.identifier))

	transcriptFile := args.identifier + "-tdd.jsonl"
	log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.TddTimeout.Minutes())))
	exitCode := runSandbox(dockerArgs, sandboxOpts{
		containerName:  containerName,
		transcriptFile: transcriptFile,
		timeout:        cfg.TddTimeout,
		verbose:        args.verbose,
		log:            log,
	})
	log.Event(fmt.Sprintf(
		"session exited (code %d) — transcript at logs/%s/%s", exitCode, runID, transcriptFile,
	))

	// Ground truth, never self-report.
	truth := gitpkg.GatherTddGroundTruth(cfg.HerdPath, slug)
	result := verify.Tdd(truth)
	if result.OK {
		plural := "s"
		if truth.CommitsAhead == 1 {
			plural = ""
		}
		log.Event(fmt.Sprintf(
			"tdd ✓ %s (%d commit%s ahead)", result.Reason, truth.CommitsAhead, plural,
		))
	} else {
		log.Event("tdd ✗ " + result.Reason)
	}

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	fileFindings(findingsDir, t.TeamID, args.identifier, client, log)

	if result.OK {
		return 0, nil
	}
	return 1, nil
}

type sandboxOpts struct {
	containerName  string
	transcriptFile string
	timeout        time.Duration
	verbose        bool
	log            *runlog.Logger
}

// runSandbox runs the sandboxed tdd session, teeing the stream-json transcript to
// disk and narrating to the console. It returns the container's exit code (1 on
// any launch failure).
func runSandbox(dockerArgs []string, opts sandboxOpts) int {
	cmd := exec.Command("docker", dockerArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		opts.log.Event("tdd ✗ failed to pipe docker stdout: " + err.Error())
		return 1
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		opts.log.Event("tdd ✗ failed to pipe docker stderr: " + err.Error())
		return 1
	}

	if err := cmd.Start(); err != nil {
		opts.log.Event("tdd ✗ failed to launch docker: " + err.Error())
		return 1
	}

	// Hard wall-clock cap: if the session overruns, kill the container out from under it.
	timer := time.AfterFunc(opts.timeout, func() {
		opts.log.Event("tdd ✗ wall-clock cap hit — killing " + opts.containerName)
		_ = exec.Command("docker", "kill", opts.containerName).Run()
	})
	defer timer.Stop()

	// Keep a bounded tail of stderr so a launch failure can be explained on the
	// console. Docker prints the real cause then a generic "See '… --help'."
	// trailer, so one line isn't enough — DockerErrorReason scans the tail.
	const stderrTailLines = 10
	var (
		stderrMu  sync.Mutex
		stderrBuf []string
	)

	var wg sync.WaitGroup
	wg.Add(2)

	// stdout carries the stream-json transcript: tee every line to disk, then either
	// echo it raw (--verbose) or surface the concise narration.
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			opts.log.TeeLine(opts.transcriptFile, line)
			if opts.verbose {
				fmt.Println(line)
				continue
			}
			if msg, ok := stream.Narrate(line); ok {
				opts.log.Event(msg)
			}
		}
	}()

	// stderr is forensic only — tee it, line-buffered to match stdout's record
	// shape — but keep the last non-empty line so we can surface docker's own
	// reason on a launch failure (otherwise it lives only in the transcript).
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			opts.log.TeeLine(opts.transcriptFile, line)
			if t := strings.TrimSpace(line); t != "" {
				stderrMu.Lock()
				stderrBuf = append(stderrBuf, t)
				if len(stderrBuf) > stderrTailLines {
					stderrBuf = stderrBuf[len(stderrBuf)-stderrTailLines:]
				}
				stderrMu.Unlock()
			}
		}
	}()

	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Exit 125 means docker couldn't start the container at all (daemon down,
	// image missing, bad flag). The reason is teed only to the transcript, so
	// echo the last stderr line to the console — otherwise the operator sees a
	// bare "code 125" with no cause (BEH-316).
	if exitCode == sandbox.ExitCannotStart {
		stderrMu.Lock()
		hint := sandbox.DockerErrorReason(strings.Join(stderrBuf, "\n"))
		stderrMu.Unlock()
		if hint == "" {
			hint = "see transcript for docker's error"
		}
		opts.log.Event("tdd ✗ docker could not start the container (exit 125): " + hint)
	}

	return exitCode
}

// execCombinedOutput runs a command and returns its combined stdout+stderr,
// matching the runner signature sandbox.Preflight expects.
func execCombinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func fileFindings(
	findingsDir, teamID, relatedIdentifier string, client *linear.Client, log *runlog.Logger,
) {
	text, err := os.ReadFile(filepath.Join(findingsDir, "out.json"))
	if err != nil {
		return // no dropbox file → nothing to file (the common, friction-free case)
	}

	parsed := findings.Parse(string(text))
	if parsed.Error != "" {
		log.Event("findings ✗ dropbox unreadable: " + parsed.Error)
		return
	}
	if len(parsed.Findings) == 0 {
		return
	}
	if teamID == "" {
		log.Event(fmt.Sprintf(
			"findings ✗ %d dropped but no team id resolved for %s",
			len(parsed.Findings), relatedIdentifier,
		))
		return
	}

	for _, f := range parsed.Findings {
		created, err := client.FileFinding(f, linear.FileFindingOptions{
			TeamID: teamID, RelatedIdentifier: relatedIdentifier,
		})
		if err != nil {
			log.Event(fmt.Sprintf("finding ✗ failed to file %q: %s", f.Title, err.Error()))
			continue
		}
		log.Event(fmt.Sprintf("finding filed: %s — %s", created.Identifier, f.Title))
	}
}

// loadDotEnv loads KEY=VALUE pairs from a .env file into the process environment
// for any key not already set (mirrors `node --env-file-if-exists`). An absent
// file is a no-op.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
