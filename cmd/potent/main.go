// Command potent is the semantic-idempotency gateway entrypoint.
//
// It loads a policy YAML, constructs an in-memory store (Week 1 default),
// and forwards inbound HTTP requests through the idempotency pipeline to
// the upstream tool server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	upstream := flag.String("upstream", "", "upstream tool server URL (required)")
	policyPath := flag.String("policy", "configs/policy.yaml", "policy YAML path")
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

	st := store.NewMemory(time.Now)
	p := proxy.New(cfg, st, upstreamURL, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", p)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("potent listening", "addr", *addr, "upstream", upstreamURL.String(), "policy", *policyPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
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
	return srv.Shutdown(shutdownCtx)
}
