package reconciler

import "github.com/prometheus/client_golang/prometheus"

// Reconcile actions recorded per instance per reconcile. They exist to
// dark-launch the reconciler: enable it with dry-run on and watch
// action="would_change" to understand the blast radius before letting it write.
const (
	actionInSync      = "in_sync"      // already matches desired; no action
	actionWouldChange = "would_change" // dry-run: a change is needed but was not applied
	actionChanged     = "changed"      // a converging patch was applied
	actionError       = "error"        // the patch failed
)

var reconcilerActionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "bifrost_reconciler_actions_total",
		Help: "Reconcile actions by outcome (drives the reconciler dark launch).",
	},
	[]string{"action"},
)

func init() {
	prometheus.MustRegister(reconcilerActionsTotal)
}
