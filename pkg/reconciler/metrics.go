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
	actionSkipped     = "skipped"      // the instance is not bifrost-managed; nothing was done
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
// instance whose intent stops resolving silently drops out of the reconciled
// set, and nothing in a rate() of the action counters shows the fleet
// shrinking.
var managedInstances = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "bifrost_reconciler_managed_instances",
		Help: "Bifrost-managed Unleash instances the reconciler is responsible for.",
	},
)

// unmanagedInstances is the other half of that denominator. Counting only
// managed instances cannot see the case the pair exists for: an instance that
// loses its managed-by label leaves numerator and denominator at once, so the
// managed gauge just steps down by one and looks like a deletion. Reported as a
// second gauge rather than a managed="true|false" label on the first, because
// bifrost_reconciler_managed_instances is already exported (the reconciler need
// not be enabled for that — see the timestamp below), and adding a label to a
// live series silently breaks every query that selects it by name alone.
var unmanagedInstances = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "bifrost_reconciler_unmanaged_instances",
		Help: "Unleash instances in bifrost's namespace without the managed-by label, which the reconciler ignores.",
	},
)

// instancesUpdatedTimestamp separates "never measured" from "measured, and the
// fleet is empty". Both gauges are registered in init, which runs in every
// bifrost process because package server imports this package — so they read 0
// from process start, and the chart ships reconciler.enabled=false. Without
// this timestamp the dark-launch step "turn the reconciler on and expect
// managed_instances = 0" cannot answer its own question, because 0 is also what
// a bifrost with no reconciler at all publishes.
//
// A timestamp is preferred over registering the gauges on first success: it is
// scraped as an ordinary series, and it additionally makes a stalled census
// alertable (time() - ..._updated_timestamp_seconds > a few resync intervals),
// which is exactly the failure mode the "a failed List keeps the previous
// value" behaviour would otherwise hide.
//
// Note for >1 replica: the census runnable is added to the manager's
// LeaderElection group, so non-leaders would publish a permanent 0 with a
// permanent 0 timestamp next to the leader's real values. The chart pins
// replicas: 1, so this is recorded rather than solved; the fix, if replicas
// grow, is to select on the leader (e.g. via the leader-election lease) rather
// than to aggregate.
var instancesUpdatedTimestamp = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "bifrost_reconciler_instances_updated_timestamp_seconds",
		Help: "Unix time of the last successful instance census; 0 means no census has completed.",
	},
)

// Adoption outcomes. Kept off reconcilerActionsTotal deliberately: that counter
// means "a reconcile happened and here is what it did", and adoption is not a
// reconcile — it is the metadata write that lets one happen at all. Mixing them
// would make sum(rate(actions_total)) stop meaning reconciles, and would put a
// one-off migration spike inside the series the dark launch is read from.
const (
	adoptionAdopted = "adopted" // the managed-by label was stamped
	adoptionError   = "error"   // the stamping patch failed
)

// adoptionsTotal makes "65 instances were adopted" an event that was observed
// rather than one inferred afterwards from unmanagedInstances falling as
// managedInstances rises. It is also what tells the two apart during a
// migration: instances left unmanaged after a sweep are opted out or failed to
// stamp, and only the error series says which.
var adoptionsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "bifrost_reconciler_adoptions_total",
		Help: "Unleash instances stamped with the bifrost managed-by label by the fleet adopter.",
	},
	[]string{"result"},
)

func init() {
	prometheus.MustRegister(reconcilerActionsTotal, managedInstances, unmanagedInstances, instancesUpdatedTimestamp, adoptionsTotal)
}

// recordAction increments the action counter. Every call site must pass a
// reason so the label is never empty; reasonNone covers the actions that have
// no drift cause.
func recordAction(action, reason string) {
	reconcilerActionsTotal.WithLabelValues(action, reason).Inc()
}
