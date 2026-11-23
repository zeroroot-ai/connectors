package fixloop

import (
	"errors"
	"fmt"
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

// The sha of the empty blob and of "hello\n" are the known values of git
// (git hash-object).
func TestBlobSHA(t *testing.T) {
	for content, want := range map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	} {
		if got := BlobSHA(content); got != want {
			t.Errorf("BlobSHA(%q) = %s, want %s", content, got, want)
		}
	}
}

func TestBranchStays(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&kit.APIError{Status: 502}, "the branch b stays, call the tool again"},
		{&kit.APIError{Status: 429}, "the branch b stays, call the tool again"},
		{errors.New("plain"), "the branch b stays, call the tool again"},
		{&kit.APIError{Status: 403, RateLimited: true}, "the branch b stays, call the tool again"},
		{&kit.APIError{Status: 403}, "the branch b stays"},
		{&kit.APIError{Status: 401}, "the branch b stays"},
		{fmt.Errorf("wrapped: %w", &kit.APIError{Status: 422}), "the branch b stays"},
	} {
		if got := BranchStays("b", c.err); got != c.want {
			t.Errorf("BranchStays(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
