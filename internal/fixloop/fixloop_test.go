package fixloop

import (
	"testing"
	"time"
)

func TestRevertBranch(t *testing.T) {
	for try, want := range map[int]string{1: "b", 2: "b-2", 3: "b-3"} {
		if got := RevertBranch("b", try); got != want {
			t.Errorf("RevertBranch(b, %d) = %s, want %s", try, got, want)
		}
	}
}

func TestTime(t *testing.T) {
	if Time(time.Time{}) != "" {
		t.Error("zero time must be empty")
	}
	in := "2026-11-20T10:00:00+02:00"
	if got := Time(ParseTime(in)); got != "2026-11-20T08:00:00Z" {
		t.Errorf("Time = %s", got)
	}
	if !ParseTime("yesterday").IsZero() {
		t.Error("bad time must give the zero time")
	}
}
