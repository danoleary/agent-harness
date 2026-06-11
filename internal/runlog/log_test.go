package runlog

import (
	"testing"
	"time"
)

func TestMakeRunIDFormatsZeroPadded(t *testing.T) {
	id := MakeRunID(time.Date(2026, 6, 11, 14, 8, 5, 0, time.UTC))
	if id != "20260611-140805" {
		t.Errorf("MakeRunID = %q, want 20260611-140805", id)
	}
}

func TestMakeRunIDSortsChronologically(t *testing.T) {
	earlier := MakeRunID(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	later := MakeRunID(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))

	if !(earlier < later) {
		t.Errorf("expected %q < %q", earlier, later)
	}
	if earlier != "20260101-000000" {
		t.Errorf("earlier = %q", earlier)
	}
	if later != "20261231-235959" {
		t.Errorf("later = %q", later)
	}
}
