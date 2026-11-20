package fixloop

import (
	"testing"
	"time"
)

func TestRefusals(t *testing.T) {
	var r Refusals
	if _, ok := r.Get("a!1", "s1"); ok {
		t.Fatal("empty memory returned a refusal")
	}
	r.Note("a!1", "s1", "not approved")
	if got, ok := r.Get("a!1", "s1"); !ok || got != "not approved" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	if _, ok := r.Get("a!1", "s2"); ok {
		t.Error("a refusal of an older head counted")
	}
	r.Clear("a!1")
	if _, ok := r.Get("a!1", "s1"); ok {
		t.Error("Clear kept the refusal")
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
