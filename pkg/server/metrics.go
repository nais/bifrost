package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Authentication outcomes recorded on every non-exempt API request. They exist
// so the PSK auth rollout can be dark-launched: while enforcement is off, watch
// authOutcomeUnauthenticated fall to zero (all callers sending a valid key)
// before flipping BIFROST_AUTH_ENFORCED to true.
const (
	authOutcomeAuthenticated   = "authenticated"
	authOutcomeUnauthenticated = "unauthenticated_allowed" // no valid key, allowed (accept mode)
	authOutcomeRejected        = "rejected"                // no valid key, rejected (enforce mode)
)

var apiAuthRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "bifrost_api_auth_requests_total",
		Help: "Total API requests by authentication outcome (drives the PSK auth dark launch).",
	},
	[]string{"outcome"},
)

func init() {
	prometheus.MustRegister(apiAuthRequestsTotal)
}

// metricsHandler serves bifrost's own metrics together with
// controller-runtime's. The framework registers on its private registry, not the
// default one, and its metrics listener is disabled to avoid a second port — so
// without gathering both here, controller_runtime_reconcile_errors_total,
// reconcile duration and workqueue_depth are collected in-process and never
// scraped.
func metricsHandler() http.Handler {
	handler := promhttp.HandlerFor(
		mergedGatherer{primary: prometheus.DefaultGatherer, secondary: ctrlmetrics.Registry},
		// A partial scrape beats no scrape. The default ErrorHandling is
		// HTTPErrorOnError, under which a single broken collector — in either
		// registry, and importing the framework's registry here doubles the
		// collectors that can break — turns /metrics into a 500 with an empty
		// body, taking both dark launches blind at the same time.
		promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError},
	)
	// Restores promhttp_metric_handler_requests_total and _in_flight, which
	// promhttp.Handler() provided for free. It is the only in-process signal
	// that the scrape handler itself is failing, and it matters more now that
	// a gather error no longer shows up as a 500.
	return promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, handler)
}

// mergedGatherer exposes secondary's metric families alongside primary's,
// keeping primary's where both define the same family and dropping the
// secondary's runtime families entirely (see isRuntimeFamily). A plain
// prometheus.Gatherers cannot be used: both registries register the Go and
// process collectors, and a duplicate family name fails the entire scrape rather
// than the offending family.
type mergedGatherer struct {
	primary   prometheus.Gatherer
	secondary prometheus.Gatherer
}

func (g mergedGatherer) Gather() ([]*dto.MetricFamily, error) {
	// Both registries return the families they did collect alongside any error,
	// and those results are kept: returning nil on error would take bifrost's
	// own dark-launch counters off the endpoint because a framework collector
	// misbehaved. The errors are aggregated and handed to promhttp, which is
	// configured to expose what it has and report the failure as an error line
	// rather than as an empty 500.
	families, primaryErr := g.primary.Gather()

	seen := make(map[string]bool, len(families))
	for _, family := range families {
		seen[family.GetName()] = true
	}

	extra, secondaryErr := g.secondary.Gather()
	for _, family := range extra {
		if seen[family.GetName()] || isRuntimeFamily(family.GetName()) {
			continue
		}
		families = append(families, family)
	}

	// Both gatherers sort their own output; the concatenation has to be sorted
	// again for the exposition format to stay canonical.
	sort.Slice(families, func(i, j int) bool { return families[i].GetName() < families[j].GetName() })
	return families, errors.Join(primaryErr, secondaryErr)
}

// isRuntimeFamily reports whether name belongs to the Go runtime or process
// collectors. Applied to the secondary registry only: controller-runtime
// installs its Go collector with WithGoCollectorRuntimeMetrics(MetricsAll),
// which contributes ~110 families the default registry does not have
// (go_godebug_non_default_behavior_*, go_memory_classes_*, go_sched_*) and grew
// the scrape body more than sixfold, every minute, for telemetry about a
// runtime bifrost already reports on. The controller_runtime_* and workqueue_*
// families — the reason the secondary registry is merged in at all — are
// untouched, as is everything the primary exports.
func isRuntimeFamily(name string) bool {
	return strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_")
}
