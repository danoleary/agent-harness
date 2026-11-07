package loop

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/stages"
)

// herdImageBuildErr is the preflight error herd's loop met on 2026-11-07 (#232):
// the base bump to agent-harness-base:0.9.0 (Node 25) has no corepack, so the
// image rebuild failed at `RUN corepack enable`.
var herdImageBuildErr = errors.New(`sandbox image "herd-agent:latest" is out of date with .agent-harness/Dockerfile and the rebuild failed — run ` +
	"`docker build -t herd-agent:latest -f .agent-harness/Dockerfile .`" + ` manually (process "/bin/sh -c corepack enable" did not complete successfully: exit code: 127)`)

// A preflight abort's ticket-released record carries the preflight's own error in
// Detail, not a fixed guess at its cause (#232).
func TestPreflightAbortRecordCarriesThePreflightError(t *testing.T) {
	r := &recorder{}
	Run(Deps{
		Host: &fakeHost{
			clearStop:     func() error { return nil },
			stopRequested: stopAfter(1),
		},
		Source: &fakeSource{next: func() (string, bool) { return "PR-1385", true }},
		Pipeline: &fakePipeline{run: func(string) stages.Result {
			return stages.Result{Err: herdImageBuildErr, Disposition: stages.PreflightAborted}
		}},
		Limits: Limits{PollInterval: time.Minute},
		Clock:  testClock{},
		Log:    r,
	})
	var rec *loopstream.Record
	for i := range r.records {
		if strings.Contains(r.records[i].Message, "Docker preflight abort") {
			rec = &r.records[i]
		}
	}
	if rec == nil {
		t.Fatalf("no preflight-abort record; events = %v", r.events)
	}
	if rec.Kind != loopstream.KindTicketReleased {
		t.Errorf("kind = %q, want %q", rec.Kind, loopstream.KindTicketReleased)
	}
	if !strings.Contains(rec.Detail, "corepack enable") || !strings.Contains(rec.Detail, "exit code: 127") {
		t.Errorf("detail = %q, want the preflight error", rec.Detail)
	}
	if strings.Contains(rec.Message, "full disk / daemon down") {
		t.Errorf("message still guesses the cause: %q", rec.Message)
	}
}

// Consecutive preflight aborts back off longer each time, so the loop does not
// rebuild a broken image every poll; an outcome past preflight resets it (#232).
func TestRepeatedPreflightAbortsBackOffLonger(t *testing.T) {
	outcomes := []stages.Result{
		{Err: herdImageBuildErr, Disposition: stages.PreflightAborted},
		{Err: herdImageBuildErr, Disposition: stages.PreflightAborted},
		{Err: herdImageBuildErr, Disposition: stages.PreflightAborted},
		{Disposition: stages.Interrupted},
		{Err: herdImageBuildErr, Disposition: stages.PreflightAborted},
	}
	var ran int
	var slept []time.Duration
	Run(Deps{
		Host: &fakeHost{
			clearStop:     func() error { return nil },
			stopRequested: stopAfter(len(outcomes)),
		},
		Source: &fakeSource{next: func() (string, bool) { return "PR-1385", true }},
		Pipeline: &fakePipeline{run: func(string) stages.Result {
			ran++
			return outcomes[ran-1]
		}},
		Limits: Limits{PollInterval: time.Minute, MaxConsecutiveFailures: 1},
		Clock:  testClock{sleep: func(d time.Duration) { slept = append(slept, d) }},
		Log:    &recorder{},
	})
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, time.Minute, time.Minute}
	if !reflect.DeepEqual(slept, want) {
		t.Errorf("backoffs = %v, want %v", slept, want)
	}
}

// The backoff stops growing at its ceiling.
func TestPreflightBackoffIsCapped(t *testing.T) {
	var slept []time.Duration
	Run(Deps{
		Host: &fakeHost{
			clearStop:     func() error { return nil },
			stopRequested: stopAfter(10),
		},
		Source: &fakeSource{next: func() (string, bool) { return "BEH-7", true }},
		Pipeline: &fakePipeline{run: func(string) stages.Result {
			return stages.Result{Err: herdImageBuildErr, Disposition: stages.PreflightAborted}
		}},
		Limits: Limits{PollInterval: time.Minute},
		Clock:  testClock{sleep: func(d time.Duration) { slept = append(slept, d) }},
		Log:    &recorder{},
	})
	if len(slept) != 10 {
		t.Fatalf("slept %d times, want 10: %v", len(slept), slept)
	}
	if got := slept[len(slept)-1]; got != maxPreflightBackoff*time.Minute {
		t.Errorf("last backoff = %v, want the ceiling %v", got, maxPreflightBackoff*time.Minute)
	}
}
