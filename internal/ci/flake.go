package ci

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// flakeLogTailLines caps how much of each check's failed log the flake record
// keeps: the failure surfaces at the tail, and a record for every flake of every
// run must stay small enough for the retrospective to read whole (#371).
const flakeLogTailLines = 100

// jobIDRE pulls the job id out of an Actions check link,
// e.g. https://github.com/owner/repo/actions/runs/100/job/2 → 2.
var jobIDRE = regexp.MustCompile(`/actions/runs/\d+/job/(\d+)`)

// runJob is one job of a run attempt, as `gh run view --json jobs` reports it.
type runJob struct {
	DatabaseID int64  `json:"databaseId"`
	Name       string `json:"name"`
	Steps      []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// failingStep is the first step of the job that concluded failure, or "" when
// none did (a job cancelled or killed before any step failed).
func (j runJob) failingStep() string {
	for _, s := range j.Steps {
		if s.Conclusion == "failure" {
			return s.Name
		}
	}
	return ""
}

// RecordFlake reads the failed attempt of each Actions run behind the checks a
// re-run washed out — the attempt before the run's latest, which the re-run
// started — and keeps its failing steps and the last flakeLogTailLines lines of
// each job's failed log (`gh run view <id> --attempt <n> --log-failed`) in the
// ticket's log dir. A retrospective cannot reach the remote, and the run log
// names only the checks, so without it nothing tells a network blip from a flaky
// test (#371). It returns each check's failing step by check name. Every gh error
// is written into the record instead of returned: the record is best-effort and
// never changes the verdict the re-run earned.
func (d *GhDriver) RecordFlake(failed []Check) map[string]string {
	steps := make(map[string]string)
	var b strings.Builder
	for _, id := range RunIDs(failed) {
		d.recordRunFlake(&b, id, checksOfRun(failed, id), steps)
	}
	if b.Len() > 0 && d.keepFlakeLog != nil {
		d.keepFlakeLog(b.String())
	}
	return steps
}

// recordRunFlake writes one run's failed attempt to b and fills in the failing
// step of each of its checks.
func (d *GhDriver) recordRunFlake(b *strings.Builder, id string, checks []Check, steps map[string]string) {
	attempt, err := d.failedAttempt(id)
	if err != nil {
		fmt.Fprintf(b, "===== run %s (flake washed out by the re-run) =====\n(could not find the failed attempt: %v)\n\n", id, err)
		return
	}
	n := strconv.Itoa(attempt)
	fmt.Fprintf(b, "===== run %s attempt %s (flake washed out by the re-run) =====\n", id, n)

	if raw, err := d.viewRun(id, "--attempt", n, "--json", "jobs"); err != nil {
		fmt.Fprintf(b, "(could not fetch the attempt's jobs: %v)\n", err)
	} else if jobs, err := parseRunJobs(raw); err != nil {
		fmt.Fprintf(b, "(could not parse the attempt's jobs: %v)\n", err)
	} else {
		for _, c := range checks {
			step := "(no failed step reported)"
			if j, ok := jobOfCheck(jobs, c); ok && j.failingStep() != "" {
				step = j.failingStep()
				steps[c.Name] = step
			}
			fmt.Fprintf(b, "check %s — failing step: %s\n", c.Name, step)
		}
	}

	raw, err := d.viewRun(id, "--attempt", n, "--log-failed")
	if err != nil {
		fmt.Fprintf(b, "(could not fetch the attempt's failed log: %v)\n\n", err)
		return
	}
	b.WriteString(tailFailedLog(string(raw), flakeLogTailLines))
	b.WriteString("\n")
}

// failedAttempt is the attempt before the run's latest: the re-run that washed
// the flake out is the latest, so the one before it is the attempt that failed.
func (d *GhDriver) failedAttempt(id string) (int, error) {
	raw, err := d.viewRun(id, "--json", "attempt")
	if err != nil {
		return 0, err
	}
	var run struct {
		Attempt int `json:"attempt"`
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return 0, fmt.Errorf("parsing the run's attempt: %w", err)
	}
	if run.Attempt < 2 {
		return 0, fmt.Errorf("the run is on attempt %d, so it has no earlier, failed attempt", run.Attempt)
	}
	return run.Attempt - 1, nil
}

func parseRunJobs(raw []byte) ([]runJob, error) {
	var run struct {
		Jobs []runJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, err
	}
	return run.Jobs, nil
}

// checksOfRun is the checks whose link names Actions run id.
func checksOfRun(checks []Check, id string) []Check {
	var of []Check
	for _, c := range checks {
		if m := runIDRE.FindStringSubmatch(c.Link); m != nil && m[1] == id {
			of = append(of, c)
		}
	}
	return of
}

// jobOfCheck finds the job a check reports: by the job id in its link, which is
// the failed attempt's job since the link came from the poll before the re-run,
// else by name.
func jobOfCheck(jobs []runJob, c Check) (runJob, bool) {
	if m := jobIDRE.FindStringSubmatch(c.Link); m != nil {
		for _, j := range jobs {
			if strconv.FormatInt(j.DatabaseID, 10) == m[1] {
				return j, true
			}
		}
	}
	for _, j := range jobs {
		if j.Name == c.Name {
			return j, true
		}
	}
	return runJob{}, false
}

// tailFailedLog keeps the last limit lines of each job's section of a
// `--log-failed` log, whose lines gh prefixes "<job>\t<step>\t…", so one noisy
// job cannot crowd another out of the record.
func tailFailedLog(log string, limit int) string {
	var order []string
	lines := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		if line == "" {
			continue
		}
		job, _, _ := strings.Cut(line, "\t")
		if _, seen := lines[job]; !seen {
			order = append(order, job)
		}
		lines[job] = append(lines[job], line)
	}
	var b strings.Builder
	for _, job := range order {
		l := lines[job]
		if len(l) > limit {
			fmt.Fprintf(&b, "----- %s: last %d of %d failed-log lines -----\n", job, limit, len(l))
			l = l[len(l)-limit:]
		} else {
			fmt.Fprintf(&b, "----- %s: failed log -----\n", job)
		}
		b.WriteString(strings.Join(l, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}
