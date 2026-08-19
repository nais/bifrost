package server

import (
	"net/http"
	"sort"

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
	return promhttp.HandlerFor(
		mergedGatherer{primary: prometheus.DefaultGatherer, secondary: ctrlmetrics.Registry},
		promhttp.HandlerOpts{},
	)
}

// mergedGatherer exposes secondary's metric families alongside primary's,
// keeping primary's where both define the same family. A plain
// prometheus.Gatherers cannot be used: both registries register the Go and
// process collectors, and a duplicate family name fails the entire scrape rather
// than the offending family.
type mergedGatherer struct {
	primary   prometheus.Gatherer
	secondary prometheus.Gatherer
}

func (g mergedGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.primary.Gather()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(families))
	for _, family := range families {
		seen[family.GetName()] = true
	}

	extra, err := g.secondary.Gather()
	if err != nil {
		return nil, err
	}
	for _, family := range extra {
		if !seen[family.GetName()] {
			families = append(families, family)
		}
	}

	// Both gatherers sort their own output; the concatenation has to be sorted
	// again for the exposition format to stay canonical.
	sort.Slice(families, func(i, j int) bool { return families[i].GetName() < families[j].GetName() })
	return families, nil
}
