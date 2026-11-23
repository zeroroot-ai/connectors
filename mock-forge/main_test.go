package main

import (
	"strings"
	"testing"
)

func TestPlainHTTPIsRefusedOffLoopback(t *testing.T) {
	for _, addr := range []string{"localhost:8080", ":8080", "0.0.0.0:8080", "10.0.0.5:8080", "[::]:8080"} {
		_, err := newServer(addr, "", "", "", "", false)
		if err == nil || !strings.Contains(err.Error(), "refuse plain HTTP") {
			t.Errorf("%s: want a refusal, got %v", addr, err)
		}
	}
}

func TestPlainHTTPIsAllowedOnLoopbackOrWithTheSwitchOrWithTLS(t *testing.T) {
	cases := map[string]struct {
		addr, hosts string
		allow       bool
	}{
		"loopback v4": {addr: "127.0.0.1:8080"},
		"loopback v6": {addr: "[::1]:8080"},
		"switch":      {addr: ":8080", allow: true},
		"self signed": {addr: ":8443", hosts: "forge.mock.example.com"},
	}
	for name, c := range cases {
		srv, err := newServer(c.addr, "", "", c.hosts, "", c.allow)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if srv.WriteTimeout == 0 || srv.ReadHeaderTimeout == 0 {
			t.Errorf("%s: the server has no write or header timeout", name)
		}
		if (c.hosts != "") != (srv.TLSConfig != nil) {
			t.Errorf("%s: TLS is %v", name, srv.TLSConfig != nil)
		}
	}
}
