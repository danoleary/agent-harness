package basegate

import (
	"testing"
	"time"
)

// A red verdict can be a flake, so it is trusted only for a while; a green one
// holds for the life of its SHA.
func TestVerdictsExpireRedButNotGreen(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	v := In(t.TempDir(), func() time.Time { return now })

	if _, ok := v.Read("aaa"); ok {
		t.Fatal("an unrecorded base has no verdict")
	}
	v.Write("aaa", Verdict{Gate: "check"})
	v.Write("bbb", Verdict{})

	if got, ok := v.Read("aaa"); !ok || got.Green() || got.Gate != "check" {
		t.Errorf("Read(aaa) = %+v, %v; want the red verdict naming its gate", got, ok)
	}
	now = now.Add(RedRecheck + time.Minute)
	if _, ok := v.Read("aaa"); ok {
		t.Error("a red verdict past RedRecheck must be gated again")
	}
	if got, ok := v.Read("bbb"); !ok || !got.Green() {
		t.Errorf("Read(bbb) = %+v, %v; a green verdict does not expire", got, ok)
	}
}
