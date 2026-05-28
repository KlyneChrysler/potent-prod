// Package metrics exposes Prometheus metrics for the potent proxy.
//
// All metrics live behind a single Registry instance so tests can construct
// an isolated registry per test case and avoid global state collisions.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Metrics groups the proxy's Prometheus collectors. Construct via New.
type Metrics struct {
	Registry        *prometheus.Registry
	Decisions       *prometheus.CounterVec
	UpstreamLatency *prometheus.HistogramVec
	StoreErrors     *prometheus.CounterVec
}

// New constructs a Metrics with all collectors registered against a fresh
// registry. Pass nil to register against the default registry.
func New(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{
		Registry: reg,
		Decisions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "potent",
				Subsystem: "proxy",
				Name:      "decisions_total",
				Help:      "Count of policy decisions partitioned by tool, mode, and outcome.",
			},
			[]string{"tool", "mode", "decision"},
		),
		UpstreamLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "potent",
				Subsystem: "proxy",
				Name:      "upstream_latency_seconds",
				Help:      "Latency of forwarded calls to the upstream tool server.",
				Buckets:   prometheus.DefBuckets,
			},
			[]string{"tool"},
		),
		StoreErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "potent",
				Subsystem: "store",
				Name:      "errors_total",
				Help:      "Errors returned by the persistence layer, partitioned by operation.",
			},
			[]string{"op"},
		),
	}
	reg.MustRegister(m.Decisions, m.UpstreamLatency, m.StoreErrors)
	return m
}

// Handler returns the http.Handler that exposes Prometheus metrics for
// scraping at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
