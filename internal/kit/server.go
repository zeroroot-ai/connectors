package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the path of the MCP endpoint. The ToolHive proxy of Gibson calls
// it, and the Dockerfile of each server exposes the port of ListenAddr.
const MCPPath = "/mcp"

// HealthPath answers 200 when the process serves requests.
const HealthPath = "/healthz"

// DefaultListenAddr is the address of the server when LISTEN_ADDR is not set.
const DefaultListenAddr = ":8080"

// Build makes the tools of one server from its configuration. It returns a
// function that registers the tools. It returns an error when the
// configuration is wrong, and the server then does not start.
type Build func(env *Env, log *slog.Logger) (register func(*mcp.Server), err error)

// NewHandler returns the HTTP handler of a server: the MCP endpoint and the
// health endpoint. The MCP session is stateless, so a restart of the pod
// loses no session.
func NewHandler(name, version string, register func(*mcp.Server), log *slog.Logger) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
	register(srv)
	mux := http.NewServeMux()
	mux.Handle(MCPPath, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log}))
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// Run reads the configuration, builds the tools, and serves until SIGINT or
// SIGTERM. It is the whole of func main of a server. It returns the exit code.
func Run(name, version string, build Build) int {
	log := NewLogger(os.Stderr, slog.LevelInfo).With("server", name, "version", version)
	env := NewEnv()
	register, err := build(env, log)
	if err == nil {
		err = env.Err()
	}
	if err != nil {
		log.Error("configuration is wrong", "error", err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := env.Optional("LISTEN_ADDR", DefaultListenAddr)
	if err := serve(ctx, addr, NewHandler(name, version, register, log), log); err != nil {
		log.Error("server stopped", "error", err.Error())
		return 1
	}
	return 0
}

func serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	hs := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
