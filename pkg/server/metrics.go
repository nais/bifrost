package server

import "github.com/prometheus/client_golang/prometheus"

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
