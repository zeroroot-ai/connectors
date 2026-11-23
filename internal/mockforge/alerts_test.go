package mockforge

import "testing"

func TestInRange(t *testing.T) {
	for _, c := range []struct {
		ver, rng string
		want     bool
	}{
		{"v1.2.0", ">= 1.0.0, < 1.2.4", true},
		{"v1.2.4", ">= 1.0.0, < 1.2.4", false},
		{"v0.9.9", ">= 1.0.0, < 1.2.4", false},
		{"1.10.0", "< 1.9", false},
		{"1.2.3", "= 1.2.3", true},
		{"1.2.3", "bad", false},
		{"1.2.3", "", false},
	} {
		if got := inRange(c.ver, c.rng); got != c.want {
			t.Errorf("inRange(%q, %q) = %v", c.ver, c.rng, got)
		}
	}
}

func TestGoModRequire(t *testing.T) {
	mod := "module m\nrequire a/b v1.0.0\nrequire (\n\tc/d v2.0.0 // indirect\n)\n"
	if goModRequire(mod, "a/b") != "v1.0.0" || goModRequire(mod, "c/d") != "v2.0.0" || goModRequire(mod, "x") != "" {
		t.Error("goModRequire")
	}
}
