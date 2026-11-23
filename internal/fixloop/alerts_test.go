package fixloop

import (
	"reflect"
	"testing"
)

func TestPurl(t *testing.T) {
	for _, c := range []struct{ eco, name, ver, want string }{
		{"npm", "lodash", "4.17.20", "pkg:npm/lodash@4.17.20"},
		{"npm", "@scope/pkg", "1.0.0", "pkg:npm/%40scope/pkg@1.0.0"},
		{"go", "github.com/x/y", "v1.2.3", "pkg:golang/github.com/x/y@v1.2.3"},
		{"go", "github.com/Azure/Go-X", "v1.2.3", "pkg:golang/github.com/azure/go-x@v1.2.3"},
		{"go", "github.com/x/y", "v2.0.0+incompatible", "pkg:golang/github.com/x/y@v2.0.0%2Bincompatible"},
		{"maven", "org.acme:core", "2.0", "pkg:maven/org.acme/core@2.0"},
		{"pip", "Django_Rest", "3.1", "pkg:pypi/django-rest@3.1"},
		{"poetry", "requests", "2.0.0", "pkg:pypi/requests@2.0.0"},
		{"uv", "requests", "2.0.0", "pkg:pypi/requests@2.0.0"},
		{"conda", "numpy", "1.0", "pkg:conda/numpy@1.0"},
		{"cargo", "serde", "1.0.1", "pkg:cargo/serde@1.0.1"},
		{"rust", "serde", "1.0.1", "pkg:cargo/serde@1.0.1"},
		{"bundler", "rails", "7.0.0", "pkg:gem/rails@7.0.0"},
		{"go", "github.com/x/y", "", ""},
		{"unknown", "a", "1", ""},
	} {
		if got := Purl(c.eco, c.name, c.ver); got != c.want {
			t.Errorf("Purl(%q,%q,%q) = %q, want %q", c.eco, c.name, c.ver, got, c.want)
		}
	}
}

func TestAdvisoryIDs(t *testing.T) {
	got := AdvisoryIDs("cve-2024-12345 and GHSA-C2F3-g4h5-j6m7", "CVE-2024-12345")
	want := []string{"GHSA-c2f3-g4h5-j6m7", "CVE-2024-12345"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := AdvisoryIDs("none"); got == nil || len(got) != 0 {
		t.Errorf("got %#v, want an empty list", got)
	}
}

func TestInstalledPackage(t *testing.T) {
	gomod := "module m\n\nrequire github.com/a/b v1.2.3 // indirect\n\nrequire (\n\tgithub.com/x/y v0.4.0\n\tgithub.com/z/w v1.0.0 // c\n" +
		"\tgithub.com/r/one v1.0.0\n\tgithub.com/r/two v1.0.0\n\tgithub.com/r/local v1.0.0\n)\n" +
		"replace github.com/r/one => github.com/fork/one v1.0.1\nreplace (\n\tgithub.com/r/two v1.0.0 => github.com/fork/two v2.0.0\n" +
		"\tgithub.com/r/local => ../local\n)\n"
	lock := `{"packages":{"":{"version":"1"},"node_modules/lodash":{"version":"4.17.20"}}}`
	nested := `{"packages":{"node_modules/lodash":{"version":"4.17.20"},"node_modules/a/node_modules/lodash":{"version":"3.10.1"}}}`
	lock1 := `{"dependencies":{"lodash":{"version":"4.17.19"}}}`
	nested1 := `{"dependencies":{"lodash":{"version":"4.17.19"},"a":{"version":"1","dependencies":{"lodash":{"version":"3.0.0"}}}}}`
	for _, c := range []struct {
		file, content, name, wantName string
		want                          []string
	}{
		{"go.mod", gomod, "github.com/a/b", "github.com/a/b", []string{"v1.2.3"}},
		{"sub/go.mod", gomod, "github.com/x/y", "github.com/x/y", []string{"v0.4.0"}},
		{"go.mod", gomod, "github.com/z/w", "github.com/z/w", []string{"v1.0.0"}},
		{"go.mod", gomod, "github.com/none", "github.com/none", nil},
		{"go.mod", gomod, "github.com/r/one", "github.com/fork/one", []string{"v1.0.1"}},
		{"go.mod", gomod, "github.com/fork/one", "github.com/fork/one", []string{"v1.0.1"}},
		{"go.mod", gomod, "github.com/r/two", "github.com/fork/two", []string{"v2.0.0"}},
		{"go.mod", gomod, "github.com/r/local", "github.com/r/local", nil},
		{"package-lock.json", lock, "lodash", "lodash", []string{"4.17.20"}},
		{"package-lock.json", nested, "lodash", "lodash", []string{"3.10.1", "4.17.20"}},
		{"package-lock.json", lock1, "lodash", "lodash", []string{"4.17.19"}},
		{"package-lock.json", nested1, "lodash", "lodash", []string{"3.0.0", "4.17.19"}},
		{"package-lock.json", "{bad", "lodash", "lodash", nil},
		{"requirements.txt", "Django_Rest==3.1 ; python_version>'3'\nflask>=2\n", "django-rest", "django-rest", []string{"3.1"}},
		{"requirements.txt", "flask===2.0.1\n", "flask", "flask", []string{"2.0.1"}},
		{"requirements.txt", "flask>=2\n", "flask", "flask", nil},
		{"requirements.txt", "flask==2.*\n", "flask", "flask", nil},
		{"requirements.txt", "flask==2.0,<3\n", "flask", "flask", nil},
		{"pom.xml", "x", "x", "x", nil},
	} {
		got := InstalledPackage(c.file, c.content, c.name)
		if got.Name != c.wantName || !reflect.DeepEqual(got.Versions, c.want) {
			t.Errorf("%s %s = %+v, want %s %v", c.file, c.name, got, c.wantName, c.want)
		}
	}
}

func TestEcosystemOfFile(t *testing.T) {
	if EcosystemOfFile("a/b/go.mod") != "go" || EcosystemOfFile("poetry.lock") != "poetry" || EcosystemOfFile("x.txt") != "" {
		t.Error("EcosystemOfFile")
	}
}

func TestCheckAlertsInput(t *testing.T) {
	if s, err := CheckAlertsInput(AlertsInput{}); err != nil || s != AlertOpen {
		t.Errorf("default: %q %v", s, err)
	}
	if _, err := CheckAlertsInput(AlertsInput{State: "bogus"}); err == nil {
		t.Error("bad state accepted")
	}
	if _, err := CheckAlertsInput(AlertsInput{Ref: "../x"}); err == nil {
		t.Error("bad ref accepted")
	}
}
