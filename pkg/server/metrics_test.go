package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// controller-runtime registers on its own registry and its metrics listener is
// disabled, so /metrics is the only place its telemetry can be scraped from.
func TestMetricsHandler_ServesBothRegistries(t *testing.T) {
	framework := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "controller_runtime_reconcile_errors_total_test",
		Help: "Stand-in for a metric controller-runtime registers on its private registry.",
	})
	ctrlmetrics.Registry.MustRegister(framework)
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(framework) })
	framework.Inc()

	apiAuthRequestsTotal.WithLabelValues(authOutcomeAuthenticated).Inc()

	w := httptest.NewRecorder()
	metricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
	}
	for _, want := range []string{
		"bifrost_api_auth_requests_total",                // default registry
		"controller_runtime_reconcile_errors_total_test", // controller-runtime's registry
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("/metrics does not expose %s", want)
		}
	}

	// Both registries carry the Go and process collectors. Exposing a family
	// twice is not merely untidy: it makes the scrape fail outright.
	if got := strings.Count(w.Body.String(), "# HELP go_goroutines "); got != 1 {
		t.Errorf("go_goroutines exposed %d times, want 1", got)
	}
}
