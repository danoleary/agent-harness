package ci

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedHead yields the given CI-run head SHAs in order (repeating the last once
// exhausted) and counts its calls — the head-poll analogue of scriptedFetch.
func scriptedHead(heads ...string) (func() (string, error), *int) {
	calls := 0
	return func() (string, error) {
		i := calls
		if i >= len(heads) {
			i = len(heads) - 1
		}
		calls++
		return heads[i], nil
	}, &calls
}

func TestAwaitHeadRunReturnsWhenCICatchesUp(t *testing.T) {
	clock := newFakeClock()
	// CI shows the prior run's head twice, then registers a run for the pushed commit.
	ciHead, calls := scriptedHead("oldsha", "oldsha", "newsha")
	if err := awaitHeadRun("newsha", ciHead, testPollCfg(), clock.sleep, clock.now); err != nil {
		t.Fatalf("awaitHeadRun: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("fetched %d times, want 3 (waited through 2 stale heads)", *calls)
	}
}

func TestAwaitHeadRunReturnsImmediatelyNoSleep(t *testing.T) {
	clock := newFakeClock()
	slept := 0
	sleep := func(d time.Duration) { slept++; clock.sleep(d) }
	ciHead, _ := scriptedHead("newsha")
	if err := awaitHeadRun("newsha", ciHead, testPollCfg(), sleep, clock.now); err != nil {
		t.Fatalf("awaitHeadRun: %v", err)
	}
	if slept != 0 {
		t.Fatalf("slept %d times, want 0 when CI already has the new head", slept)
	}
}

func TestAwaitHeadRunTimesOutIfCINeverReruns(t *testing.T) {
	clock := newFakeClock()
	ciHead, _ := scriptedHead("oldsha") // never catches up to the pushed commit
	err := awaitHeadRun("newsha", ciHead, testPollCfg(), clock.sleep, clock.now)
	if !errors.Is(err, ErrCIRerunTimeout) {
		t.Fatalf("err = %v, want ErrCIRerunTimeout", err)
	}
}

func TestAwaitHeadRunTreatsNoRunYetAsNotCaughtUp(t *testing.T) {
	clock := newFakeClock()
	// "" = Actions hasn't registered any run for the branch yet; keep waiting, then
	// converge once the run for the pushed commit appears.
	ciHead, calls := scriptedHead("", "", "newsha")
	if err := awaitHeadRun("newsha", ciHead, testPollCfg(), clock.sleep, clock.now); err != nil {
		t.Fatalf("awaitHeadRun: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("fetched %d times, want 3", *calls)
	}
}

func TestAwaitHeadRunPropagatesFetchError(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("gh run list: auth required")
	ciHead := func() (string, error) { return "", boom }
	err := awaitHeadRun("newsha", ciHead, testPollCfg(), clock.sleep, clock.now)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}

func TestInterpretRunHeadParsesLatestSha(t *testing.T) {
	head, err := interpretRunHead([]byte(`[{"headSha":"abc123"}]`), nil, nil)
	if err != nil {
		t.Fatalf("interpretRunHead: %v", err)
	}
	if head != "abc123" {
		t.Fatalf("head = %q, want abc123", head)
	}
}

func TestInterpretRunHeadEmptyListMeansNoRunYet(t *testing.T) {
	head, err := interpretRunHead([]byte(`[]`), nil, nil)
	if err != nil {
		t.Fatalf("interpretRunHead: %v", err)
	}
	if head != "" {
		t.Fatalf("head = %q, want empty (no run registered yet)", head)
	}
}

func TestInterpretRunHeadSurfacesGhFailure(t *testing.T) {
	_, err := interpretRunHead([]byte(""), []byte("gh: authentication required"), errors.New("exit status 4"))
	if err == nil {
		t.Fatal("expected an error when stdout has no parseable JSON")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error should surface gh stderr, got %q", err.Error())
	}
}
