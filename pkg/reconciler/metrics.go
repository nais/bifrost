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
	actionIntentError = "intent_error" // the desired-state intent could not be resolved; instance skipped
)

// Why an instance is not in sync. Without this, would_change says how many
// instances differ but never what differs, and the three causes have very
// different meanings: a missing annotation is benign backfill, a spec mismatch
// may be destructive. The set is deliberately closed and small — it is a metric
// label, so every value multiplies the series count.
const (
	reasonNone           = "none"                   // in sync, or the action has no drift cause
	reasonSpecMismatch   = "spec_mismatch"          // the live spec differs from the render
	reasonMissingLabel   = "missing_managed_label"  // the managed-by label is absent
	reasonIntentMismatch = "desired_state_mismatch" // the desired-state annotation differs
)

var reconcilerActionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "bifrost_reconciler_actions_total",
		Help: "Reconcile actions by outcome and drift cause (drives the reconciler dark launch).",
	},
	[]string{"action", "reason"},
)

// managedInstances is the denominator the action counters are missing: an
// instance whose intent stops resolving, or that loses its managed-by label,
// silently drops out of the reconciled set, and nothing in a rate() of the
// action counters shows the fleet shrinking.
var managedInstances = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "bifrost_reconciler_managed_instances",
		Help: "Bifrost-managed Unleash instances the reconciler is responsible for.",
	},
)

func init() {
	prometheus.MustRegister(reconcilerActionsTotal, managedInstances)
}

// recordAction increments the action counter. Every call site must pass a
// reason so the label is never empty; reasonNone covers the actions that have
// no drift cause.
func recordAction(action, reason string) {
	reconcilerActionsTotal.WithLabelValues(action, reason).Inc()
}
