// Command potent is the semantic-idempotency gateway entrypoint.
//
// It loads a policy YAML, constructs the configured store backend, exposes
// Prometheus metrics, and runs inbound requests through the idempotency
// pipeline. Three modes are supported:
//
//   - http: generic HTTP reverse proxy keyed on the X-Potent-Tool header
//   - mcp-http: MCP Streamable HTTP, intercepts JSON-RPC tools/call frames
//   - mcp-stdio: spawns an MCP server as a child and proxies its stdio
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/potent/potent/internal/admin"
	"github.com/potent/potent/internal/audit"
	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/mcp"
	"github.com/potent/potent/internal/metrics"
	"github.com/potent/potent/internal/pipeline"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/proxy"
	"github.com/potent/potent/internal/store"
)

// Build metadata, populated by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("potent %s (commit %s, built %s)\n", version, commit, date)
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "http", "operating mode: http | mcp-http | mcp-stdio")
	addr := flag.String("addr", ":8080", "listen address (http and mcp-http)")
	metricsAddr := flag.String("metrics-addr", ":9090", "metrics listen address (http and mcp-http)")
	upstream := flag.String("upstream", "", "upstream URL (http, mcp-http) or command (mcp-stdio)")
	policyPath := flag.String("policy", "configs/policy.yaml", "policy YAML path")
	backend := flag.String("store", "memory", "store backend: memory | bolt")
	dbPath := flag.String("db", "potent.db", "BoltDB file path (when -store=bolt)")
	embedDim := flag.Int("embed-dim", 384, "embedding dimension")
	embedN := flag.Int("embed-ngram", 4, "character n-gram size for the embedder")
	auditPath := flag.String("audit-log", "", "append JSONL audit records to this path (empty = disabled)")
	adminAddr := flag.String("admin-addr", "", "admin HTTP API listen address (empty = disabled; recommend 127.0.0.1:9095)")
	maxBodyBytes := flag.Int64("max-body-bytes", 1<<20, "cap on inbound tool-call request bodies (0 = unlimited; recommend leaving the default)")
	stdioTimeout := flag.Duration("stdio-request-timeout", 60*time.Second, "how long an in-flight mcp-stdio tools/call may wait for a response before being treated as failed")
	compactInterval := flag.Duration("bolt-compact-interval", time.Hour, "how often the bolt store sweeps expired entries from disk (0 = disabled)")
	flag.Parse()

	apiToken := os.Getenv("POTENT_API_TOKEN")

	adminToken := os.Getenv("POTENT_ADMIN_TOKEN")

	// Log to stderr. In mcp-stdio mode, stdout is the JSON-RPC protocol
	// channel and any non-protocol byte on it corrupts the stream. Stderr
	// is the conventional log sink anyway, so all modes get it.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *upstream == "" {
		return errors.New("flag -upstream is required")
	}

	cfg, err := policy.Load(*policyPath)
	if err != nil {
		return err
	}

	st, closeStore, err := openStore(*backend, *dbPath, logger)
	if err != nil {
		return err
	}
	defer closeStore()

	// Start the bolt compactor in a background goroutine so on-disk entries
	// past their TTL are reclaimed even when never accessed via Get.
	if b, ok := st.(*store.Bolt); ok && *compactInterval > 0 {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		go b.RunCompactor(ctx, *compactInterval, func(removed int, cerr error) {
			if cerr != nil {
				logger.Warn("bolt compactor", "err", cerr)
				return
			}
			if removed > 0 {
				logger.Info("bolt compact", "removed", removed)
			}
		})
	}

	emb, err := embed.NewHashingTFIDF(*embedDim, *embedN)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}

	var auditWriter *audit.Writer
	if *auditPath != "" {
		auditWriter, err = audit.Open(*auditPath, 1024)
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = auditWriter.Close(ctx)
		}()
	} else {
		auditWriter = audit.NewDiscard()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = auditWriter.Close(ctx)
		}()
	}

	m := metrics.New(nil)
	pl := pipeline.New(cfg, st,
		pipeline.WithMetrics(m),
		pipeline.WithEmbedder(emb),
		pipeline.WithAudit(auditWriter),
	)

	if *adminAddr != "" {
		if adminToken == "" {
			return errors.New("admin server requested via -admin-addr but POTENT_ADMIN_TOKEN is empty; refusing to expose unauthenticated admin endpoints")
		}
		if !isLoopbackBind(*adminAddr) {
			logger.Warn("admin-addr is not bound to loopback; ensure mTLS or network policy gates this port",
				"admin_addr", *adminAddr)
		}
		go runAdmin(*adminAddr, st, adminToken, logger)
	}

	// Refuse to start an unauthenticated proxy listener on a non-loopback
	// address. mcp-stdio is single-tenant (the client and child share the
	// parent process) so the check does not apply.
	if (*mode == "http" || *mode == "mcp-http") && apiToken == "" && !isLoopbackBind(*addr) {
		return fmt.Errorf("refusing to expose unauthenticated proxy on %q; set POTENT_API_TOKEN or bind -addr to loopback", *addr)
	}

	switch *mode {
	case "http":
		return runHTTP(*addr, *metricsAddr, *upstream, pl, m, logger, *maxBodyBytes, apiToken)
	case "mcp-http":
		return runMCPHTTP(*addr, *metricsAddr, *upstream, pl, m, logger, *maxBodyBytes, apiToken)
	case "mcp-stdio":
		return runMCPStdio(*upstream, pl, logger, *stdioTimeout)
	default:
		return fmt.Errorf("unknown mode %q (want http | mcp-http | mcp-stdio)", *mode)
	}
}

func runHTTP(addr, metricsAddr, upstream string, pl *pipeline.Pipeline, m *metrics.Metrics, logger *slog.Logger, maxBody int64, apiToken string) error {
	u, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	p := proxy.New(pl, u, logger, proxy.WithMaxBodyBytes(maxBody), proxy.WithAPIToken(apiToken))
	return serveDual(addr, metricsAddr, p.Handler(), m, "http", upstream, logger)
}

func runMCPHTTP(addr, metricsAddr, upstream string, pl *pipeline.Pipeline, m *metrics.Metrics, logger *slog.Logger, maxBody int64, apiToken string) error {
	u, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	h := mcp.NewHTTPHandler(pl, u, logger, mcp.WithHTTPMaxBodyBytes(maxBody), mcp.WithHTTPAPIToken(apiToken))
	return serveDual(addr, metricsAddr, h.Handler(), m, "mcp-http", upstream, logger)
}

func runMCPStdio(cmd string, pl *pipeline.Pipeline, logger *slog.Logger, timeout time.Duration) error {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return errors.New("mcp-stdio: -upstream must be a command to exec")
	}
	h := mcp.NewStdioHandler(pl, parts[0], parts[1:], logger)
	h.SetRequestTimeout(timeout)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("potent mcp-stdio starting", "cmd", cmd, "request_timeout", timeout)
	return h.Run(ctx, os.Stdin, os.Stdout)
}

func serveDual(addr, metricsAddr string, app http.Handler, m *metrics.Metrics, mode, upstream string, logger *slog.Logger) error {
	mux := http.NewServeMux()
	// healthz is liveness: the process is up. readyz is readiness: the
	// process is willing to accept traffic. Splitting them lets k8s
	// distinguish "restart me" from "stop sending traffic".
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/", app)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", m.Handler())

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("potent listening", "mode", mode, "addr", addr, "upstream", upstream)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	go func() {
		logger.Info("metrics server listening", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	return srv.Shutdown(shutdownCtx)
}

func runAdmin(addr string, st store.Store, token string, logger *slog.Logger) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           admin.Handler(st, st, token),
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("admin api listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("admin server", "err", err)
	}
}

// isLoopbackBind reports whether addr binds the listener to localhost only
// (any of 127.0.0.1, ::1, localhost). Used to surface a warning when the
// admin port would be reachable on a non-loopback interface.
func isLoopbackBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func openStore(backend, dbPath string, logger *slog.Logger) (store.Store, func(), error) {
	switch backend {
	case "memory":
		return store.NewMemory(time.Now), func() {}, nil
	case "bolt":
		b, err := store.OpenBolt(dbPath, time.Now)
		if err != nil {
			return nil, nil, fmt.Errorf("open bolt store: %w", err)
		}
		closer := func() {
			if err := b.Close(); err != nil {
				logger.Warn("close bolt store", "err", err)
			}
		}
		return b, closer, nil
	default:
		return nil, nil, fmt.Errorf("unknown store backend %q", backend)
	}
}
