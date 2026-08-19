package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testConfig() *config.Config {
	return &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:           "bifrost-unleash",
			InstanceServiceaccount:      "sa",
			SQLInstanceID:               "sql",
			SQLInstanceAddress:          "10.0.0.1",
			InstanceWebIngressHost:      "web.example",
			InstanceWebIngressClass:     "web-class",
			InstanceAPIIngressHost:      "api.example",
			InstanceAPIIngressClass:     "api-class",
			InstanceWebOAuthJWTAudience: "aud",
			TeamsApiURL:                 "https://teams.example",
			TeamsApiSecretName:          "teams",
			TeamsApiSecretTokenKey:      "token",
		},
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := unleashv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newReconciler(c client.Client) *UnleashReconciler {
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	return NewUnleashReconciler(c, testConfig(), logger, time.Minute, false)
}

func requestFor(crd *unleashv1.Unleash) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: crd.Namespace, Name: crd.Name}}
}

// intentFor builds a config the way production does — through the builder, so
// it carries the defaults and passes validation. Building a bare unleash.Config
// here would produce an intent the reconciler now rejects, and would not
// resemble anything the API path ever writes.
func intentFor(t *testing.T, name string) *unleash.Config {
	t.Helper()
	cfg, err := unleash.NewConfigBuilder().
		WithName(name).
		WithCustomVersion("1.2.3").
		WithFederation("fixed-nonce", "", "", "").
		Build()
	if err != nil {
		t.Fatalf("building intent: %v", err)
	}
	return cfg
}

// renderManaged produces a bifrost-managed, annotated CRD (with a fixed nonce so
// rendering is deterministic).
func renderManaged(t *testing.T, name string) unleashv1.Unleash {
	t.Helper()
	return kubernetes.BuildUnleashCRD(testConfig(), intentFor(t, name))
}

func TestReconcile_ConvergesDrift(t *testing.T) {
	desired := renderManaged(t, "team-a")
	drifted := desired.DeepCopy()
	drifted.Spec.ApiIngress.Class = "DRIFTED"         // an operator/manual edit
	drifted.Spec.WebIngress.Host = "hijacked.example" // another drift

	c := newFakeClient(t, drifted)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), requestFor(drifted)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: drifted.Namespace, Name: "team-a"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.ApiIngress.Class != "api-class" {
		t.Errorf("ApiIngress.Class = %q, want %q (drift not corrected)", got.Spec.ApiIngress.Class, "api-class")
	}
	if got.Spec.WebIngress.Host != desired.Spec.WebIngress.Host {
		t.Errorf("WebIngress.Host = %q, want %q (drift not corrected)", got.Spec.WebIngress.Host, desired.Spec.WebIngress.Host)
	}
}

func TestReconcile_IgnoresUnmanaged(t *testing.T) {
	desired := renderManaged(t, "team-b")
	foreign := desired.DeepCopy()
	delete(foreign.Labels, kubernetes.LabelManagedBy) // not bifrost-managed
	foreign.Spec.ApiIngress.Class = "hand-authored"

	c := newFakeClient(t, foreign)
	r := newReconciler(c)

	if _, err := r.Reconcile(context.Background(), requestFor(foreign)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: foreign.Namespace, Name: "team-b"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.ApiIngress.Class != "hand-authored" {
		t.Errorf("unmanaged instance was modified: class = %q", got.Spec.ApiIngress.Class)
	}
}

func TestReconcile_NoOpWhenInSync(t *testing.T) {
	desired := renderManaged(t, "team-c")
	obj := desired.DeepCopy()

	c := newFakeClient(t, obj)
	r := newReconciler(c)

	before := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: obj.Namespace, Name: "team-c"}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), requestFor(obj)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: obj.Namespace, Name: "team-c"}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("in-sync reconcile issued a write (RV %s -> %s)", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestReconcile_DryRunObservesWithoutWriting(t *testing.T) {
	desired := renderManaged(t, "team-d")
	drifted := desired.DeepCopy()
	drifted.Spec.ApiIngress.Class = "DRIFTED"

	c := newFakeClient(t, drifted)
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	r := NewUnleashReconciler(c, testConfig(), logger, time.Minute, true) // dry-run

	before := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: drifted.Namespace, Name: "team-d"}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}
	wouldChangeBefore := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionWouldChange, reasonSpecMismatch))

	if _, err := r.Reconcile(context.Background(), requestFor(drifted)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: drifted.Namespace, Name: "team-d"}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("dry-run must not write (RV %s -> %s)", before.ResourceVersion, after.ResourceVersion)
	}
	if after.Spec.ApiIngress.Class != "DRIFTED" {
		t.Errorf("dry-run changed the object: class = %q", after.Spec.ApiIngress.Class)
	}
	if got := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionWouldChange, reasonSpecMismatch)); got != wouldChangeBefore+1 {
		t.Errorf("would_change counter = %v, want %v", got, wouldChangeBefore+1)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// applyCRDDefaults mimics what the API server does to a stored Unleash object:
// it stamps the CRD's declared defaults, and a JSON round-trip turns rendered
// empty slices into nil. The fake client does neither, which is precisely why
// the original convergence bugs were invisible to the existing tests.
func applyCRDDefaults(t *testing.T, crd *unleashv1.Unleash) *unleashv1.Unleash {
	t.Helper()

	raw, err := json.Marshal(crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stored := &unleashv1.Unleash{}
	if err := json.Unmarshal(raw, stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// +kubebuilder:default fields the API server fills in when absent.
	stored.Spec.Prometheus.Enabled = true
	stored.Spec.NetworkPolicy.Enabled = true
	stored.Spec.NetworkPolicy.AllowDNS = true
	if stored.Spec.Size == 0 {
		stored.Spec.Size = 1
	}

	return stored
}

// The reconciler must reach a fixed point: an instance created through the
// normal path, once stored by the API server, must reconcile to in_sync with no
// write. Without this the loop patches every instance on every resync forever
// while converging on nothing, and dry-run reports permanent drift for the whole
// fleet.
func TestReconcile_FreshlyCreatedInstanceIsAFixedPoint(t *testing.T) {
	cfg := testConfig()

	// Render exactly as the create path does, including minting the nonce.
	created := kubernetes.BuildUnleashCRD(cfg, intentFor(t, "team-a"))
	stored := applyCRDDefaults(t, &created)

	scheme := runtime.NewScheme()
	if err := unleashv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).Build()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	r := NewUnleashReconciler(c, cfg, logger, time.Minute, false)

	before := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionInSync, reasonNone))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "team-a", Namespace: cfg.Unleash.InstanceNamespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionInSync, reasonNone))
	if after != before+1 {
		t.Fatalf("a freshly created instance must reconcile as in_sync; in_sync counter went %v → %v", before, after)
	}

	// And the stored object must be untouched.
	live := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "team-a", Namespace: cfg.Unleash.InstanceNamespace,
	}, live); err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.ResourceVersion != stored.ResourceVersion {
		t.Fatalf("in-sync reconcile must not write (resourceVersion %s → %s)", stored.ResourceVersion, live.ResourceVersion)
	}
}

// The desired-state annotation is authoritative, so a valid-JSON-but-wrong
// annotation would be rendered verbatim onto a live instance. Malformed JSON
// already failed; these are the cases that used to get through.
func TestResolveIntent_RejectsUnusableAnnotations(t *testing.T) {
	cfg := testConfig()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	r := NewUnleashReconciler(nil, cfg, logger, time.Minute, true)

	for _, tc := range []struct {
		name, annotation, wantErr string
	}{
		{
			name:       "empty object renders nothing usable",
			annotation: `{}`,
			wantErr:    "not a valid config",
		},
		{
			name:       "annotation belonging to another instance",
			annotation: `{"Name":"other-team","LogLevel":"warn","DatabasePoolMax":3}`,
			wantErr:    "names \"other-team\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd := &unleashv1.Unleash{}
			crd.SetName("team-a")
			crd.SetAnnotations(map[string]string{kubernetes.AnnotationDesiredState: tc.annotation})

			_, err := r.resolveIntent(crd)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// A well-formed annotation for this instance must still be accepted.
func TestResolveIntent_AcceptsValidAnnotation(t *testing.T) {
	cfg := testConfig()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	r := NewUnleashReconciler(nil, cfg, logger, time.Minute, true)

	crd := &unleashv1.Unleash{}
	crd.SetName("team-a")
	crd.SetAnnotations(map[string]string{
		kubernetes.AnnotationDesiredState: `{"Name":"team-a","LogLevel":"warn","DatabasePoolMax":3,"ReleaseChannelName":"stable"}`,
	})

	got, err := r.resolveIntent(crd)
	if err != nil {
		t.Fatalf("valid annotation rejected: %v", err)
	}
	if got.Name != "team-a" {
		t.Fatalf("Name = %q, want team-a", got.Name)
	}
}

// The whole point of the reason label: a would_change reading has to be
// attributable to a cause without re-rendering the instance by hand.
func TestInSync_ReportsDriftCause(t *testing.T) {
	desired := renderManaged(t, "team-e")

	cases := []struct {
		name     string
		mutate   func(*unleashv1.Unleash)
		inSync   bool
		reason   string
		sections []string
	}{
		{
			name:   "identical",
			mutate: func(*unleashv1.Unleash) {},
			inSync: true,
			reason: reasonNone,
		},
		{
			name:     "spec drift",
			mutate:   func(u *unleashv1.Unleash) { u.Spec.Size = 3 },
			reason:   reasonSpecMismatch,
			sections: []string{"Size"},
		},
		{
			name: "drift in several sections",
			mutate: func(u *unleashv1.Unleash) {
				u.Spec.Size = 3
				u.Spec.ApiIngress.Class = "DRIFTED"
				u.Spec.Federation.SecretNonce = "rotated"
			},
			reason:   reasonSpecMismatch,
			sections: []string{"Size", "ApiIngress", "Federation"},
		},
		{
			name:   "managed-by label removed",
			mutate: func(u *unleashv1.Unleash) { delete(u.Labels, kubernetes.LabelManagedBy) },
			reason: reasonMissingLabel,
		},
		{
			name:   "desired-state annotation not yet stamped",
			mutate: func(u *unleashv1.Unleash) { delete(u.Annotations, kubernetes.AnnotationDesiredState) },
			reason: reasonIntentMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := desired.DeepCopy()
			tc.mutate(live)

			got := newReconciler(newFakeClient(t)).inSync(live, &desired)
			if got.inSync != tc.inSync {
				t.Errorf("inSync = %v, want %v", got.inSync, tc.inSync)
			}
			if got.reason != tc.reason {
				t.Errorf("reason = %q, want %q", got.reason, tc.reason)
			}
			if !equality.Semantic.DeepEqual(got.sections, tc.sections) {
				t.Errorf("sections = %v, want %v", got.sections, tc.sections)
			}
		})
	}
}

// An instance whose intent cannot be resolved drops out of the managed set. It
// must appear in a metric, not only in a log line.
func TestReconcile_CountsUnresolvableIntent(t *testing.T) {
	desired := renderManaged(t, "team-f")
	broken := desired.DeepCopy()
	broken.Annotations[kubernetes.AnnotationDesiredState] = "{not json"

	c := newFakeClient(t, broken)
	r := newReconciler(c)

	before := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionIntentError, reasonNone))

	res, err := r.Reconcile(context.Background(), requestFor(broken))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %v, want the resync interval", res.RequeueAfter)
	}
	if got := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionIntentError, reasonNone)); got != before+1 {
		t.Errorf("intent_error counter = %v, want %v", got, before+1)
	}

	live := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: broken.Namespace, Name: "team-f"}, live); err != nil {
		t.Fatalf("get: %v", err)
	}
	if live.ResourceVersion != broken.ResourceVersion {
		t.Errorf("an unresolvable intent must not write (RV %s -> %s)", broken.ResourceVersion, live.ResourceVersion)
	}
}

// The gauge is the denominator for the action counters, so it must count
// bifrost-managed instances only.
func TestCountManagedInstances_CountsOnlyManaged(t *testing.T) {
	managedA := renderManaged(t, "team-g")
	managedB := renderManaged(t, "team-h")
	foreign := renderManaged(t, "team-i")
	delete(foreign.Labels, kubernetes.LabelManagedBy)

	c := newFakeClient(t, &managedA, &managedB, &foreign)
	r := newReconciler(c)

	r.countManagedInstances(context.Background())

	if got := testutil.ToFloat64(managedInstances); got != 2 {
		t.Errorf("managed_instances = %v, want 2", got)
	}
}
