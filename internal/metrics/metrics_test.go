package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestNew_RegistersAllCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	m.Decisions.WithLabelValues("send_email", "strict", "forward").Inc()
	m.UpstreamLatency.WithLabelValues("send_email").Observe(0.123)
	m.StoreErrors.WithLabelValues("get").Inc()

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	for _, want := range []string{
		`potent_proxy_decisions_total{decision="forward",mode="strict",tool="send_email"} 1`,
		`potent_proxy_upstream_latency_seconds_count{tool="send_email"} 1`,
		`potent_store_errors_total{op="get"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestNew_NilRegistryUsesFresh(t *testing.T) {
	m := New(nil)
	if m.Registry == nil {
		t.Errorf("Registry must not be nil")
	}
}
