package fixloop

import (
	"errors"
	"strings"
	"testing"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

func str(s string) *string { return &s }

func ok() OpenInput {
	return OpenInput{Repo: "a/b", SourceBranch: "fix/x-1", TargetBranch: "main", Title: "Fix x",
		Changes: []FileChange{{Path: "src/a.go", Content: str("package a")}, {Path: "old.txt", Delete: true}}}
}

func TestCheckOpenAccepts(t *testing.T) {
	if err := CheckOpen(ok()); err != nil {
		t.Fatal(err)
	}
	in := ok()
	in.Changes = []FileChange{{Path: "empty.txt", Content: str("")}}
	if err := CheckOpen(in); err != nil {
		t.Errorf("an empty file: %v", err)
	}
}

func TestCheckOpenRefuses(t *testing.T) {
	cases := map[string]func(*OpenInput){
		"same branch":      func(i *OpenInput) { i.SourceBranch = "main" },
		"dot dot branch":   func(i *OpenInput) { i.SourceBranch = "a..b" },
		"hidden segment":   func(i *OpenInput) { i.SourceBranch = "a/.b" },
		"lock suffix":      func(i *OpenInput) { i.SourceBranch = "a.lock" },
		"space in branch":  func(i *OpenInput) { i.SourceBranch = "a b" },
		"flag branch":      func(i *OpenInput) { i.SourceBranch = "-x" },
		"empty segment":    func(i *OpenInput) { i.TargetBranch = "a//b" },
		"empty title":      func(i *OpenInput) { i.Title = "" },
		"long title":       func(i *OpenInput) { i.Title = strings.Repeat("a", MaxTitleLen+1) },
		"no changes":       func(i *OpenInput) { i.Changes = nil },
		"too many changes": func(i *OpenInput) { i.Changes = make([]FileChange, MaxChanges+1) },
		"dot dot path":     func(i *OpenInput) { i.Changes[0].Path = "../x" },
		"absolute path":    func(i *OpenInput) { i.Changes[0].Path = "/x" },
		"git folder":       func(i *OpenInput) { i.Changes[0].Path = "a/.git/config" },
		"backslash path":   func(i *OpenInput) { i.Changes[0].Path = `a\b` },
		"both":             func(i *OpenInput) { i.Changes[0].Delete = true },
		"none":             func(i *OpenInput) { i.Changes[0].Content = nil },
		"same path twice":  func(i *OpenInput) { i.Changes[1].Path = i.Changes[0].Path },
		"long file":        func(i *OpenInput) { i.Changes[0].Content = str(strings.Repeat("a", MaxContentLen+1)) },
	}
	for name, mutate := range cases {
		in := ok()
		mutate(&in)
		if err := CheckOpen(in); err == nil || !errors.Is(err, kit.ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
