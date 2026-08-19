package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// seriesValue reads one series out of the registry by the metric name and label
// values as they are actually exposed. Tests go through this rather than
// testutil.ToFloat64(vec.WithLabelValues(actionX, reasonY)) because the latter
// compares the constants against themselves: renaming every action and reason
// to the empty string keeps such a suite green while the exposed metric becomes
// unusable. Names and label values are therefore spelled out as literals here.
func seriesValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !hasLabels(metric, labels) {
				continue
			}
			switch {
			case metric.Counter != nil:
				return metric.Counter.GetValue()
			case metric.Gauge != nil:
				return metric.Gauge.GetValue()
			default:
				t.Fatalf("metric %s is neither a counter nor a gauge", name)
			}
		}
	}
	// Not exported yet: a counter with no observation, which reads as zero.
	return 0
}

func hasLabels(metric *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		got[pair.GetName()] = pair.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for name, value := range want {
		if got[name] != value {
			return false
		}
	}
	return true
}

func actionCount(t *testing.T, action, reason string) float64 {
	t.Helper()
	return seriesValue(t, "bifrost_reconciler_actions_total", map[string]string{"action": action, "reason": reason})
}

// The action and reason constants are a wire contract: dashboards, the rollout
// runbook and the alert rules all select on these strings. Asserting them
// against the constants would be a tautology, so they are written out.
func TestReconcilerActions_ExposeTheDocumentedLabelValues(t *testing.T) {
	cases := []struct {
		action, reason string
		record         func()
	}{
		{"in_sync", "none", func() { recordAction(actionInSync, reasonNone) }},
		{"would_change", "spec_mismatch", func() { recordAction(actionWouldChange, reasonSpecMismatch) }},
		{"would_change", "desired_state_mismatch", func() { recordAction(actionWouldChange, reasonIntentMismatch) }},
		{"would_change", "missing_desired_state", func() { recordAction(actionWouldChange, reasonMissingIntent) }},
		{"changed", "spec_mismatch", func() { recordAction(actionChanged, reasonSpecMismatch) }},
		{"error", "spec_mismatch", func() { recordAction(actionError, reasonSpecMismatch) }},
		{"intent_error", "none", func() { recordAction(actionIntentError, reasonNone) }},
		{"skipped", "missing_managed_label", func() { recordAction(actionSkipped, reasonMissingLabel) }},
	}

	for _, tc := range cases {
		t.Run(tc.action+"/"+tc.reason, func(t *testing.T) {
			before := actionCount(t, tc.action, tc.reason)
			tc.record()
			if got := actionCount(t, tc.action, tc.reason); got != before+1 {
				t.Errorf(`bifrost_reconciler_actions_total{action=%q,reason=%q} = %v, want %v`,
					tc.action, tc.reason, got, before+1)
			}
		})
	}
}

// The census has to see the whole namespace, not just the instances that still
// carry the label — an instance that loses it must show up as unmanaged rather
// than simply vanish.
func TestCountInstances_PartitionsTheNamespace(t *testing.T) {
	managedA := renderManaged(t, "team-g")
	managedB := renderManaged(t, "team-h")
	unmanaged := renderManaged(t, "team-i")
	delete(unmanaged.Labels, kubernetes.LabelManagedBy)

	r := newReconciler(newFakeClient(t, &managedA, &managedB, &unmanaged))

	start := time.Now()
	r.countInstances(context.Background())

	if got := seriesValue(t, "bifrost_reconciler_managed_instances", nil); got != 2 {
		t.Errorf("bifrost_reconciler_managed_instances = %v, want 2", got)
	}
	if got := seriesValue(t, "bifrost_reconciler_unmanaged_instances", nil); got != 1 {
		t.Errorf("bifrost_reconciler_unmanaged_instances = %v, want 1", got)
	}
	if got := seriesValue(t, "bifrost_reconciler_instances_updated_timestamp_seconds", nil); got < float64(start.Unix()) {
		t.Errorf("bifrost_reconciler_instances_updated_timestamp_seconds = %v, want >= %v", got, start.Unix())
	}
}

// Instances in other namespaces belong to another bifrost (or to nobody); the
// census must not count them as this bifrost's unmanaged strays.
func TestCountInstances_IgnoresOtherNamespaces(t *testing.T) {
	managed := renderManaged(t, "team-j")
	elsewhere := renderManaged(t, "team-k")
	elsewhere.Namespace = "someone-else"
	delete(elsewhere.Labels, kubernetes.LabelManagedBy)

	r := newReconciler(newFakeClient(t, &managed, &elsewhere))
	r.countInstances(context.Background())

	if got := seriesValue(t, "bifrost_reconciler_unmanaged_instances", nil); got != 0 {
		t.Errorf("bifrost_reconciler_unmanaged_instances = %v, want 0", got)
	}
}

// A failed List must leave the last good reading in place — a zero would read as
// "the fleet disappeared" — and must not advance the freshness timestamp, which
// is the only thing that then says the reading is stale.
func TestCountInstances_FailedListKeepsPreviousValues(t *testing.T) {
	managed := renderManaged(t, "team-l")
	r := newReconciler(newFakeClient(t, &managed))
	r.countInstances(context.Background())

	managedBefore := seriesValue(t, "bifrost_reconciler_managed_instances", nil)
	stampBefore := seriesValue(t, "bifrost_reconciler_instances_updated_timestamp_seconds", nil)
	if managedBefore != 1 {
		t.Fatalf("precondition: managed = %v, want 1", managedBefore)
	}

	scheme := runtime.NewScheme()
	if err := addSchemeForTest(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()

	r = newReconciler(failing)
	r.countInstances(context.Background())

	if got := seriesValue(t, "bifrost_reconciler_managed_instances", nil); got != managedBefore {
		t.Errorf("a failed census overwrote the gauge: %v -> %v", managedBefore, got)
	}
	if got := seriesValue(t, "bifrost_reconciler_instances_updated_timestamp_seconds", nil); got != stampBefore {
		t.Errorf("a failed census advanced the freshness timestamp: %v -> %v", stampBefore, got)
	}
}
