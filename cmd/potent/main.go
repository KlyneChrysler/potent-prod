// Command potent is the semantic-idempotency gateway entrypoint.
//
// It loads a policy YAML, constructs the configured store backend
// (memory or bolt), exposes Prometheus metrics, and forwards inbound
// HTTP requests through the idempotency pipeline to the upstream tool
// server.
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
	"syscall"
	"time"

	"github.com/potent/potent/internal/metrics"
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
	addr := flag.String("addr", ":8080", "listen address")
	metricsAddr := flag.String("metrics-addr", ":9090", "metrics listen address")
	upstream := flag.String("upstream", "", "upstream tool server URL (required)")
	policyPath := flag.String("policy", "configs/policy.yaml", "policy YAML path")
	backend := flag.String("store", "memory", "store backend: memory | bolt")
	dbPath := flag.String("db", "potent.db", "BoltDB file path (when -store=bolt)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *upstream == "" {
		return errors.New("flag -upstream is required")
	}
	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		return err
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

	m := metrics.New(nil)
	p := proxy.New(cfg, st, upstreamURL, logger, proxy.WithMetrics(m))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", p)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", m.Handler())

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	metricsSrv := &http.Server{Addr: *metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("potent proxy listening", "addr", *addr, "upstream", upstreamURL.String(), "policy", *policyPath, "store", *backend)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	go func() {
		logger.Info("metrics server listening", "addr", *metricsAddr)
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
