// Command mock-forge is a TEST image. It is a stateful mock of the GitHub REST
// API and of the Prometheus HTTP API, for the exit test of the fix loop. It
// holds no real data and checks no credential. Never run it in a production
// install.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zeroroot-ai/connectors/internal/kit"
	"github.com/zeroroot-ai/connectors/internal/mockforge"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() { os.Exit(run()) }

func run() int {
	log := kit.NewLogger(os.Stderr, slog.LevelInfo).With("server", "mock-forge", "version", Version)
	env := kit.NewEnv()
	addr := env.Optional("LISTEN_ADDR", kit.DefaultListenAddr)
	certFile, keyFile := env.Optional("MOCK_TLS_CERT_FILE", ""), env.Optional("MOCK_TLS_KEY_FILE", "")
	hosts := env.Optional("MOCK_TLS_SELF_SIGNED_HOSTS", "")
	caOut := env.Optional("MOCK_TLS_CA_FILE", "")
	allowPlain := env.Optional("MOCK_ALLOW_PLAINTEXT", "") == "1"
	if err := env.Err(); err != nil {
		log.Error("configuration is wrong", "error", err.Error())
		return 2
	}
	srv, err := newServer(addr, certFile, keyFile, hosts, caOut, allowPlain)
	if err != nil {
		log.Error("configuration is wrong", "error", err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Warn("listening. This is a test image: it checks no credential.", "addr", addr, "tls", srv.TLSConfig != nil)
		if srv.TLSConfig != nil {
			errc <- srv.ListenAndServeTLS("", "")
			return
		}
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		log.Error("server stopped", "error", err.Error())
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("shutdown failed", "error", err.Error())
		return 1
	}
	return 0
}

// isLoopback is true when the listen address has a literal loopback IP. A name
// such as localhost is not accepted, because the code does not resolve it. An
// empty host listens on every interface, so it is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newServer makes the HTTP server. TLS is on when the environment names a
// certificate, or hosts for a certificate that the mock makes. In the second
// case the PEM of the authority goes to caOut, and GET /_mock/ca.pem serves it.
func newServer(addr, certFile, keyFile, hosts, caOut string, allowPlain bool) (*http.Server, error) {
	if certFile == "" && keyFile == "" && hosts == "" && !allowPlain && !isLoopback(addr) {
		return nil, fmt.Errorf("refuse plain HTTP on %q: it has no credential, so anyone who reaches the port can reset it. "+
			"Set MOCK_TLS_CERT_FILE and MOCK_TLS_KEY_FILE, or MOCK_TLS_SELF_SIGNED_HOSTS, or MOCK_ALLOW_PLAINTEXT=1, or listen on a loopback address", addr)
	}
	mock := mockforge.New(nil)
	mux := http.NewServeMux()
	mux.Handle("/", mock)
	srv := &http.Server{
		Addr: addr, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	switch {
	case certFile != "" || keyFile != "":
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load the certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	case hosts != "":
		cert, ca, err := mockforge.SelfSigned(strings.Split(hosts, ","), time.Now())
		if err != nil {
			return nil, fmt.Errorf("make the certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		mux.HandleFunc("/_mock/ca.pem", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write(ca)
		})
		if caOut != "" {
			if err := os.WriteFile(caOut, ca, 0o644); err != nil { //nolint:gosec // a public certificate
				return nil, fmt.Errorf("write the authority: %w", err)
			}
		}
	}
	return srv, nil
}
