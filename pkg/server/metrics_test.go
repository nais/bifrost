package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
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
		// promhttp.Handler() used to instrument itself; the hand-rolled handler
		// has to keep doing it, or the "is the scrape handler erroring" signal
		// is gone at the moment it starts mattering.
		"promhttp_metric_handler_requests_total",
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

// Which registry wins a name collision is the merge's whole contract: the
// primary is bifrost's own, and its go_/process_ families must be the ones
// exposed. Swapping the two registries has to be a visible change.
func TestMergedGatherer_KeepsThePrimarysFamily(t *testing.T) {
	const name = "merged_gatherer_collision_test_total"

	primary := prometheus.NewRegistry()
	primaryCounter := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "primary"})
	primary.MustRegister(primaryCounter)
	primaryCounter.Add(1)

	secondary := prometheus.NewRegistry()
	secondaryCounter := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "secondary"})
	secondary.MustRegister(secondaryCounter)
	secondaryCounter.Add(7)

	families, err := mergedGatherer{primary: primary, secondary: secondary}.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var seen int
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		seen++
		if got := family.GetMetric()[0].GetCounter().GetValue(); got != 1 {
			t.Errorf("%s = %v, want the primary's 1 (the secondary's is 7)", name, got)
		}
	}
	if seen != 1 {
		t.Errorf("%s exposed %d times, want 1", name, seen)
	}
}

// The exposition format requires families in name order, and the merge appends
// the secondary's families after the primary's — so without the re-sort the
// output is only accidentally canonical.
func TestMergedGatherer_SortsTheMergedOutput(t *testing.T) {
	primary := prometheus.NewRegistry()
	primary.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "zzz_primary_test_total", Help: "z"}))

	secondary := prometheus.NewRegistry()
	secondary.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "aaa_secondary_test_total", Help: "a"}))

	families, err := mergedGatherer{primary: primary, secondary: secondary}.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("families are not sorted by name: %v", names)
	}
}

// The framework's Go collector is configured with MetricsAll, which adds ~110
// families the default registry does not have. They are filtered out of the
// secondary registry only.
func TestMergedGatherer_DropsTheSecondarysRuntimeFamilies(t *testing.T) {
	primary := prometheus.NewRegistry()
	primary.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "go_kept_from_primary_test", Help: "primary runtime family"}))

	secondary := prometheus.NewRegistry()
	for _, name := range []string{"go_sched_latencies_seconds_test", "process_open_fds_test", "workqueue_depth_test"} {
		secondary.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "from the framework"}))
	}

	families, err := mergedGatherer{primary: primary, secondary: secondary}.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}
	for _, name := range []string{"go_sched_latencies_seconds_test", "process_open_fds_test"} {
		if got[name] {
			t.Errorf("%s came through from the secondary registry", name)
		}
	}
	// The families the merge exists for, and the primary's own runtime output,
	// must survive the filter.
	for _, name := range []string{"workqueue_depth_test", "go_kept_from_primary_test"} {
		if !got[name] {
			t.Errorf("%s was filtered out", name)
		}
	}
}

// One broken collector must not blank the endpoint. ContinueOnError alone does
// not achieve that: if Gather discards the families it did collect, promhttp
// has nothing to write and 500s on the empty output anyway.
func TestMetricsHandler_SurvivesABrokenCollector(t *testing.T) {
	for _, tc := range []struct {
		name     string
		registry prometheus.Registerer
	}{
		{"broken collector on the framework registry", ctrlmetrics.Registry},
		{"broken collector on bifrost's own registry", prometheus.DefaultRegisterer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := brokenCollector{}
			tc.registry.MustRegister(bad)
			t.Cleanup(func() { tc.registry.Unregister(bad) })

			apiAuthRequestsTotal.WithLabelValues(authOutcomeAuthenticated).Inc()

			w := httptest.NewRecorder()
			metricsHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "bifrost_api_auth_requests_total") {
				t.Errorf("bifrost's own metrics disappeared with the broken collector (body: %s)", w.Body.String())
			}
		})
	}
}

// ...and the error itself is not swallowed: it is reported to the caller, which
// is how promhttp logs it and counts the failed scrape.
func TestMergedGatherer_ReportsErrorsFromBothRegistries(t *testing.T) {
	healthy := prometheus.NewRegistry()
	healthy.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "healthy_family_test_total", Help: "fine"}))

	for _, tc := range []struct{ name, broken string }{
		{"primary broken", "primary"},
		{"secondary broken", "secondary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := prometheus.NewRegistry()
			broken.MustRegister(brokenCollector{})

			g := mergedGatherer{primary: healthy, secondary: broken}
			if tc.broken == "primary" {
				g = mergedGatherer{primary: broken, secondary: healthy}
			}

			families, err := g.Gather()
			if err == nil {
				t.Fatal("a broken collector must be reported, not swallowed")
			}
			if !strings.Contains(err.Error(), "collector exploded") {
				t.Errorf("error = %q, want it to name the collector failure", err)
			}

			var found bool
			for _, family := range families {
				if family.GetName() == "healthy_family_test_total" {
					found = true
				}
			}
			if !found {
				t.Errorf("the healthy registry's families were discarded along with the error")
			}
		})
	}
}

// brokenCollector stands in for a collector that fails at scrape time, the way
// a framework collector reading /proc or a stale informer can.
type brokenCollector struct{}

func (brokenCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- brokenDesc
}

func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(brokenDesc, errors.New("collector exploded"))
}

var brokenDesc = prometheus.NewDesc("broken_collector_test", "always fails at collect time", nil, nil)
