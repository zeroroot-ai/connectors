package mockforge_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeroroot-ai/connectors/internal/mockforge"
	"github.com/zeroroot-ai/connectors/internal/mockforge/mocktest"
)

func TestMergeChecksTheExpectedHead(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"a.txt": "one\n"}, nil)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "fix", "files": map[string]string{"a.txt": "two\n"}})
	srv.Post("/repos/acme/shop/pulls", map[string]any{"title": "t", "head": "fix", "base": "main"})
	status, out := srv.Do(http.MethodPut, "/repos/acme/shop/pulls/1/merge", map[string]any{"sha": "0000000000000000000000000000000000000000"})
	if status != http.StatusConflict {
		t.Fatalf("merge with a wrong sha: HTTP %d %v", status, out)
	}
	if status, out = srv.Do(http.MethodPut, "/repos/acme/shop/pulls/1/merge", map[string]any{}); status != http.StatusOK || out["merged"] != true {
		t.Fatalf("merge: HTTP %d %v", status, out)
	}
	if status, _ = srv.Do(http.MethodPut, "/repos/acme/shop/pulls/1/merge", map[string]any{}); status != http.StatusMethodNotAllowed {
		t.Errorf("merge twice: HTTP %d, want 405", status)
	}
}

func TestConflictMakesThePullRequestDirty(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"a.txt": "one\n"}, nil)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "fix", "files": map[string]string{"a.txt": "two\n"}})
	srv.Post("/repos/acme/shop/pulls", map[string]any{"title": "t", "head": "fix", "base": "main"})
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "main", "files": map[string]string{"a.txt": "three\n"}})
	_, pr := srv.Do(http.MethodGet, "/repos/acme/shop/pulls/1", nil)
	if pr["mergeable_state"] != "dirty" || pr["mergeable"] != false {
		t.Errorf("pull request with a conflict: %v", pr)
	}
	if status, _ := srv.Do(http.MethodPut, "/repos/acme/shop/pulls/1/merge", map[string]any{}); status != http.StatusMethodNotAllowed {
		t.Errorf("merge with a conflict: HTTP %d, want 405", status)
	}
}

func TestUnknownRepositoryIsNotFound(t *testing.T) {
	srv := mocktest.Start(t)
	if status, _ := srv.Do(http.MethodGet, "/repos/none/none", nil); status != http.StatusNotFound {
		t.Errorf("HTTP %d, want 404", status)
	}
	if status, _ := srv.Do(http.MethodPost, "/_mock/repos", map[string]any{"owner": "a/b", "repo": "c"}); status != http.StatusBadRequest {
		t.Errorf("seed with a slash: HTTP %d, want 400", status)
	}
}

func TestSeedTwiceAndReset(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", nil, nil)
	if status, _ := srv.Do(http.MethodPost, "/_mock/repos", map[string]any{"owner": "acme", "repo": "shop"}); status != http.StatusConflict {
		t.Errorf("seed twice: HTTP %d, want 409", status)
	}
	srv.Post("/_mock/reset", nil)
	if status, _ := srv.Do(http.MethodGet, "/repos/acme/shop", nil); status != http.StatusNotFound {
		t.Errorf("after reset: HTTP %d, want 404", status)
	}
}

func TestSelfSignedCertificateIsTrustedWithItsAuthority(t *testing.T) {
	cert, ca, err := mockforge.SelfSigned([]string{"forge.mock.example.com", "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(mockforge.New(nil))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("the authority is not PEM")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/healthz", http.NoBody)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("a client with the authority: %v", err)
	}
	_ = res.Body.Close()
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/healthz", http.NoBody)
	if res, err = plain.Do(req); err == nil {
		_ = res.Body.Close()
		t.Error("a client with no authority must refuse the certificate")
	}
}

func TestPullListIsShort(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"a.txt": "one\n"}, nil)
	srv.Post("/_mock/repos/acme/shop/commits", map[string]any{"branch": "fix", "files": map[string]string{"a.txt": "two\n"}})
	srv.Post("/repos/acme/shop/pulls", map[string]any{"title": "t", "head": "fix", "base": "main"})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/repos/acme/shop/pulls?state=all&head=acme:fix", http.NoBody)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var list []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	for _, k := range []string{"merged", "mergeable", "mergeable_state", "commits"} {
		if _, ok := list[0][k]; ok {
			t.Errorf("a list item has %s", k)
		}
	}
	for _, k := range []string{"number", "state", "merged_at", "head", "base"} {
		if _, ok := list[0][k]; !ok {
			t.Errorf("a list item has no %s", k)
		}
	}
	_, one := srv.Do(http.MethodGet, "/repos/acme/shop/pulls/1", nil)
	if _, ok := one["mergeable_state"]; !ok {
		t.Error("a single pull request has no mergeable_state")
	}
}

func TestDeploymentListHonorsPerPage(t *testing.T) {
	srv := mocktest.Start(t)
	srv.Seed("acme", "shop", map[string]any{"a.txt": "one\n"}, nil)
	for range 60 {
		srv.Post("/_mock/repos/acme/shop/deployments", map[string]any{"branch": "main", "environment": "production"})
	}
	for _, c := range []struct {
		query string
		want  int
	}{{"per_page=50", 50}, {"", 30}, {"per_page=100", 60}} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/repos/acme/shop/deployments?"+c.query, http.NoBody)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var list []any
		err = json.NewDecoder(res.Body).Decode(&list)
		_ = res.Body.Close()
		if err != nil || len(list) != c.want {
			t.Errorf("%q: %d deployments, want %d (%v)", c.query, len(list), c.want, err)
		}
	}
}
