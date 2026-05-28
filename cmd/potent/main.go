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

func main() {
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
	adminAddr := flag.String("admin-addr", "", "admin HTTP API listen address (empty = disabled)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
		go runAdmin(*adminAddr, st, logger)
	}

	switch *mode {
	case "http":
		return runHTTP(*addr, *metricsAddr, *upstream, pl, m, logger)
	case "mcp-http":
		return runMCPHTTP(*addr, *metricsAddr, *upstream, pl, m, logger)
	case "mcp-stdio":
		return runMCPStdio(*upstream, pl, logger)
	default:
		return fmt.Errorf("unknown mode %q (want http | mcp-http | mcp-stdio)", *mode)
	}
}

func runHTTP(addr, metricsAddr, upstream string, pl *pipeline.Pipeline, m *metrics.Metrics, logger *slog.Logger) error {
	u, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	p := proxy.New(pl, u, logger)
	return serveDual(addr, metricsAddr, p, m, "http", upstream, logger)
}

func runMCPHTTP(addr, metricsAddr, upstream string, pl *pipeline.Pipeline, m *metrics.Metrics, logger *slog.Logger) error {
	u, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	h := mcp.NewHTTPHandler(pl, u, logger)
	return serveDual(addr, metricsAddr, h, m, "mcp-http", upstream, logger)
}

func runMCPStdio(cmd string, pl *pipeline.Pipeline, logger *slog.Logger) error {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return errors.New("mcp-stdio: -upstream must be a command to exec")
	}
	h := mcp.NewStdioHandler(pl, parts[0], parts[1:], logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("potent mcp-stdio starting", "cmd", cmd)
	return h.Run(ctx, os.Stdin, os.Stdout)
}

func serveDual(addr, metricsAddr string, app http.Handler, m *metrics.Metrics, mode, upstream string, logger *slog.Logger) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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

func runAdmin(addr string, st store.Store, logger *slog.Logger) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           admin.Handler(st, st),
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("admin api listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("admin server", "err", err)
	}
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
