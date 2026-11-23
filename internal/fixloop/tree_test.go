package fixloop

import "testing"

func TestTreeType(t *testing.T) {
	for _, c := range []struct{ mode, typ, want string }{
		{"100644", "blob", TreeFile}, {"100755", "blob", TreeFile}, {"120000", "blob", TreeSymlink},
		{"040000", "tree", TreeDir}, {"160000", "commit", TreeSubmodule}, {"100644", "weird", ""},
	} {
		if got := TreeType(c.mode, c.typ); got != c.want {
			t.Errorf("TreeType(%s,%s) = %q, want %q", c.mode, c.typ, got, c.want)
		}
	}
}

func TestCheckTree(t *testing.T) {
	if p, err := CheckTree(TreeInput{Path: "src/db/"}); err != nil || p != "src/db" {
		t.Errorf("path: %q %v", p, err)
	}
	for name, in := range map[string]TreeInput{
		"dot dot":    {Path: "a/../b"},
		"git folder": {Path: ".git/hooks"},
		"bad ref":    {Ref: "../x"},
		"bad cursor": {Cursor: "a b;c"},
	} {
		if _, err := CheckTree(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
