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
		{"maven", "org.acme:core", "2.0", "pkg:maven/org.acme/core@2.0"},
		{"pip", "Django_Rest", "3.1", "pkg:pypi/django-rest@3.1"},
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

func TestInstalledVersion(t *testing.T) {
	gomod := "module m\n\nrequire github.com/a/b v1.2.3 // indirect\n\nrequire (\n\tgithub.com/x/y v0.4.0\n\tgithub.com/z/w v1.0.0 // c\n)\n"
	lock := `{"packages":{"":{"version":"1"},"node_modules/lodash":{"version":"4.17.20"}}}`
	lock1 := `{"dependencies":{"lodash":{"version":"4.17.19"}}}`
	for _, c := range []struct{ file, content, name, want string }{
		{"go.mod", gomod, "github.com/a/b", "v1.2.3"},
		{"sub/go.mod", gomod, "github.com/x/y", "v0.4.0"},
		{"go.mod", gomod, "github.com/z/w", "v1.0.0"},
		{"go.mod", gomod, "github.com/none", ""},
		{"package-lock.json", lock, "lodash", "4.17.20"},
		{"package-lock.json", lock1, "lodash", "4.17.19"},
		{"package-lock.json", "{bad", "lodash", ""},
		{"requirements.txt", "Django_Rest==3.1 ; python_version>'3'\nflask>=2\n", "django-rest", "3.1"},
		{"requirements.txt", "flask>=2\n", "flask", ""},
		{"pom.xml", "x", "x", ""},
	} {
		if got := InstalledVersion(c.file, c.content, c.name); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.file, c.name, got, c.want)
		}
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
