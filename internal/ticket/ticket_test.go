package ticket

import (
	"errors"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

func TestCheckCreate(t *testing.T) {
	ok := CreateInput{Title: "t", Description: "d", Priority: High, Labels: []string{"zeroroot", "a:b"}}
	if err := CheckCreate(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*CreateInput){
		"no title":       func(i *CreateInput) { i.Title = "" },
		"long title":     func(i *CreateInput) { i.Title = strings.Repeat("a", MaxTitle+1) },
		"title newline":  func(i *CreateInput) { i.Title = "a\nb" },
		"no description": func(i *CreateInput) { i.Description = "  " },
		"control char":   func(i *CreateInput) { i.Description = "a\x00b" },
		"bad priority":   func(i *CreateInput) { i.Priority = "urgent" },
		"bad label":      func(i *CreateInput) { i.Labels = []string{"a b"} },
		"many labels":    func(i *CreateInput) { i.Labels = make([]string, MaxLabels+1) },
	}
	for name, f := range bad {
		in := ok
		f(&in)
		if err := CheckCreate(in); !errors.Is(err, kit.ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A description may hold new lines.
	in := ok
	in.Description = "line 1\nline 2\ttab"
	if err := CheckCreate(in); err != nil {
		t.Errorf("new lines: %v", err)
	}
}

func TestCheckUpdateAndSearch(t *testing.T) {
	if err := CheckUpdate(UpdateInput{TicketID: "x"}); !errors.Is(err, kit.ErrInvalidInput) {
		t.Errorf("empty update: %v", err)
	}
	if err := CheckUpdate(UpdateInput{Priority: Low}); err != nil {
		t.Errorf("priority update: %v", err)
	}
	for _, in := range []SearchInput{{Status: "x"}, {Label: "a b"}, {CreatedAfter: "2026-11-20"}, {Limit: 51}, {Limit: -1}, {TitleContains: "a\nb"}} {
		if _, err := CheckSearch(in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	if l, err := CheckSearch(SearchInput{}); err != nil || l != DefaultLimit {
		t.Errorf("default: %d %v", l, err)
	}
	if StatusOf(SearchInput{}) != StatusOpen || StatusOf(SearchInput{Status: StatusAny}) != StatusAny {
		t.Error("StatusOf")
	}
}
