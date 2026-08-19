package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unadopted mirrors what the 65 production instances actually look like: created
// by bifrost before the label existed, so no managed-by label and no
// desired-state annotation.
func unadopted(t *testing.T, name string) *unleashv1.Unleash {
	t.Helper()
	rendered := renderManaged(t, name)
	crd := rendered.DeepCopy()
	delete(crd.Labels, kubernetes.LabelManagedBy)
	delete(crd.Annotations, kubernetes.AnnotationDesiredState)
	return crd
}

func newAdopter(c client.Client, cfg *config.Config) *UnleashReconciler {
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	return NewUnleashReconciler(c, cfg, logger, time.Minute, true)
}

func get(t *testing.T, c client.Client, ns, name string) *unleashv1.Unleash {
	t.Helper()
	live := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, live); err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	return live
}

// The core of the feature, and its single hardest constraint: the label makes an
// instance visible, the annotation would make a lossy read-back authoritative.
func TestAdoptFleet_StampsTheLabelAndNothingElse(t *testing.T) {
	crd := unadopted(t, "team-adopt")
	specBefore := crd.Spec.DeepCopy()

	c := newFakeClient(t, crd)
	r := newAdopter(c, testConfig())

	before := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted))

	r.adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if !kubernetes.IsManagedByBifrost(live) {
		t.Errorf("managed-by = %q, want %q", live.GetLabels()[kubernetes.LabelManagedBy], kubernetes.ManagedByBifrost)
	}
	if got, ok := live.GetAnnotations()[kubernetes.AnnotationDesiredState]; ok {
		t.Errorf("adoption wrote the desired-state annotation (%q); it must never record an intent derived from a lossy read-back", got)
	}
	if !equality.Semantic.DeepEqual(*specBefore, live.Spec) {
		t.Error("adoption modified the spec; it must be metadata-only")
	}
	if got := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted)); got != before+1 {
		t.Errorf("adoptions_total{result=adopted} = %v, want %v", got, before+1)
	}
}

// An operator must be able to keep a hand-made or test instance out without a
// code change or a release.
func TestAdoptFleet_HonoursTheOptOutLabel(t *testing.T) {
	optedOut := unadopted(t, "team-optout")
	optedOut.Labels[kubernetes.LabelAdopt] = kubernetes.AdoptOptOut

	// Any other value leaves the instance eligible: adoption is additive and
	// undone by removing a label, so an unrecognised value should fail toward
	// being visible rather than silently exempt.
	optedIn := unadopted(t, "team-optin")
	optedIn.Labels[kubernetes.LabelAdopt] = "true"

	c := newFakeClient(t, optedOut, optedIn)
	newAdopter(c, testConfig()).adoptFleet(context.Background())

	if live := get(t, c, optedOut.Namespace, optedOut.Name); kubernetes.IsManagedByBifrost(live) {
		t.Errorf("%s was adopted despite %s=%s", optedOut.Name, kubernetes.LabelAdopt, kubernetes.AdoptOptOut)
	}
	if live := get(t, c, optedIn.Namespace, optedIn.Name); !kubernetes.IsManagedByBifrost(live) {
		t.Errorf("%s was not adopted; only the exact value %q opts out", optedIn.Name, kubernetes.AdoptOptOut)
	}
}

// Stamping over another controller's claim would hand one object to two
// reconcilers.
func TestAdoptFleet_LeavesForeignlyManagedInstances(t *testing.T) {
	foreign := unadopted(t, "team-foreign")
	foreign.Labels[kubernetes.LabelManagedBy] = "unleasherator"

	c := newFakeClient(t, foreign)
	newAdopter(c, testConfig()).adoptFleet(context.Background())

	live := get(t, c, foreign.Namespace, foreign.Name)
	if got := live.GetLabels()[kubernetes.LabelManagedBy]; got != "unleasherator" {
		t.Errorf("managed-by = %q, want it left as %q", got, "unleasherator")
	}
}

// An empty namespace is metav1.NamespaceAll, so an unguarded adopter would stamp
// bifrost's label on every Unleash CR in the cluster.
func TestAdoptFleet_RefusesAnEmptyNamespace(t *testing.T) {
	for _, ns := range []string{"", "   "} {
		crd := unadopted(t, "team-empty-ns")
		c := newFakeClient(t, crd)

		cfg := testConfig()
		cfg.Unleash.InstanceNamespace = ns
		r := newAdopter(c, cfg)

		before := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted))

		r.adoptFleet(context.Background())

		if live := get(t, c, crd.Namespace, crd.Name); kubernetes.IsManagedByBifrost(live) {
			t.Errorf("adopted an instance with the configured namespace set to %q", ns)
		}
		if got := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted)); got != before {
			t.Errorf("adoptions_total{result=adopted} = %v with namespace %q, want it unchanged at %v", got, ns, before)
		}
	}
}

func TestAdoptFleet_StaysInItsOwnNamespace(t *testing.T) {
	mine := unadopted(t, "team-mine")
	theirs := unadopted(t, "team-theirs")
	theirs.Namespace = "some-tenant-namespace"

	c := newFakeClient(t, mine, theirs)
	newAdopter(c, testConfig()).adoptFleet(context.Background())

	if live := get(t, c, mine.Namespace, mine.Name); !kubernetes.IsManagedByBifrost(live) {
		t.Error("instance in the configured namespace was not adopted")
	}
	if live := get(t, c, theirs.Namespace, theirs.Name); kubernetes.IsManagedByBifrost(live) {
		t.Errorf("instance in %s was adopted; adoption must be namespace-scoped", theirs.Namespace)
	}
}

// The sweep runs every resync, so a second pass over an adopted fleet must be a
// no-op rather than 65 more counted adoptions.
func TestAdoptFleet_IsIdempotent(t *testing.T) {
	crd := unadopted(t, "team-twice")
	c := newFakeClient(t, crd)
	r := newAdopter(c, testConfig())

	r.adoptFleet(context.Background())
	after := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted))
	rv := get(t, c, crd.Namespace, crd.Name).ResourceVersion

	r.adoptFleet(context.Background())

	if got := testutil.ToFloat64(adoptionsTotal.WithLabelValues(adoptionAdopted)); got != after {
		t.Errorf("adoptions_total{result=adopted} = %v after a second sweep, want %v", got, after)
	}
	if got := get(t, c, crd.Namespace, crd.Name).ResourceVersion; got != rv {
		t.Errorf("second sweep wrote (resourceVersion %s -> %s)", rv, got)
	}
}

// Adoption writes, so it must not happen just because the reconciler is on.
func TestFleetSweep_AdoptsOnlyWhenAutoAdoptIsOn(t *testing.T) {
	for _, tt := range []struct {
		name      string
		autoAdopt bool
		want      bool
	}{
		{"off", false, false},
		{"on", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			crd := unadopted(t, "team-toggle")
			c := newFakeClient(t, crd)

			cfg := testConfig()
			cfg.Reconciler.AutoAdopt = tt.autoAdopt
			r := newAdopter(c, cfg)

			// A cancelled context runs exactly one iteration of the sweep and
			// then returns, which is the whole loop body under test.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := r.runFleetSweep(ctx); err != nil {
				t.Fatalf("runFleetSweep: %v", err)
			}

			if got := kubernetes.IsManagedByBifrost(get(t, c, crd.Namespace, crd.Name)); got != tt.want {
				t.Errorf("adopted = %v, want %v with autoAdopt=%v", got, tt.want, tt.autoAdopt)
			}
		})
	}
}

// The sweep adopts and then counts, in one pass, so a census never reports a
// fleet the sweep has not finished with: after one iteration the instances it
// just stamped are already on the managed side of the split.
func TestFleetSweep_CountsTheFleetAfterAdopting(t *testing.T) {
	first := unadopted(t, "team-sweep-a")
	second := unadopted(t, "team-sweep-b")
	optedOut := unadopted(t, "team-sweep-c")
	optedOut.Labels[kubernetes.LabelAdopt] = kubernetes.AdoptOptOut

	cfg := testConfig()
	cfg.Reconciler.AutoAdopt = true
	r := newAdopter(newFakeClient(t, first, second, optedOut), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.runFleetSweep(ctx); err != nil {
		t.Fatalf("runFleetSweep: %v", err)
	}

	if got := seriesValue(t, "bifrost_reconciler_managed_instances", nil); got != 2 {
		t.Errorf("bifrost_reconciler_managed_instances = %v, want 2; the census ran before adoption finished", got)
	}
	// The remainder is the opt-out, and it stays visible as unmanaged rather
	// than disappearing from both sides of the split.
	if got := seriesValue(t, "bifrost_reconciler_unmanaged_instances", nil); got != 1 {
		t.Errorf("bifrost_reconciler_unmanaged_instances = %v, want 1", got)
	}
}

// allActions sums every (action, reason) series, so "the reconciler recorded
// nothing beyond the skip" can be asserted without listing the label
// combinations.
func allActions(t *testing.T) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	reconcilerActionsTotal.Collect(ch)
	close(ch)

	var total float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		total += pb.GetCounter().GetValue()
	}
	return total
}

// The point of the whole exercise: before adoption the reconciler short-circuits
// on the missing label, so the only thing it can record is that it skipped the
// instance — a count of instances it knows nothing about, which is exactly what
// a fleet with no drift also looks like. Adoption is what turns that skip into a
// measurement of the instance itself.
func TestAdoptFleet_TurnsAnInvisibleInstanceIntoAMeasuredOne(t *testing.T) {
	crd := unadopted(t, "team-visible")
	c := newFakeClient(t, crd)
	r := newAdopter(c, testConfig())

	beforeAll := allActions(t)
	beforeSkipped := actionCount(t, actionSkipped, reasonMissingLabel)
	if _, err := r.Reconcile(context.Background(), requestFor(crd)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := actionCount(t, actionSkipped, reasonMissingLabel); got != beforeSkipped+1 {
		t.Fatalf("skipped/missing_managed_label = %v, want %v", got, beforeSkipped+1)
	}
	if got := allActions(t); got != beforeAll+1 {
		t.Fatalf("an unadopted instance recorded %v actions past the skip; it is invisible to the reconciler by design", got-beforeAll-1)
	}

	r.adoptFleet(context.Background())

	beforeAll = allActions(t)
	beforeSkipped = actionCount(t, actionSkipped, reasonMissingLabel)
	if _, err := r.Reconcile(context.Background(), requestFor(crd)); err != nil {
		t.Fatalf("reconcile after adoption: %v", err)
	}
	if got := actionCount(t, actionSkipped, reasonMissingLabel); got != beforeSkipped {
		t.Error("an adopted instance was still skipped for a missing managed-by label")
	}
	if got := allActions(t); got <= beforeAll {
		t.Error("an adopted instance recorded no reconcile action; adoption did not make it visible")
	}
}
