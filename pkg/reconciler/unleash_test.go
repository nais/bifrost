package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
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

// renderManaged produces a bifrost-managed, annotated CRD (with a fixed nonce so
// rendering is deterministic).
func renderManaged(name string) unleashv1.Unleash {
	cfg := &unleash.Config{Name: name, CustomVersion: "1.2.3", FederationNonce: "fixed-nonce", EnableFederation: true}
	return kubernetes.BuildUnleashCRD(testConfig(), cfg)
}

func TestReconcile_ConvergesDrift(t *testing.T) {
	desired := renderManaged("team-a")
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
	desired := renderManaged("team-b")
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
	desired := renderManaged("team-c")
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
	desired := renderManaged("team-d")
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
	wouldChangeBefore := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionWouldChange))

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
	if got := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionWouldChange)); got != wouldChangeBefore+1 {
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
	intent := &unleash.Config{Name: "team-a", EnableFederation: true}
	created := kubernetes.BuildUnleashCRD(cfg, intent)
	stored := applyCRDDefaults(t, &created)

	scheme := runtime.NewScheme()
	if err := unleashv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).Build()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	r := NewUnleashReconciler(c, cfg, logger, time.Minute, false)

	before := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionInSync))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "team-a", Namespace: cfg.Unleash.InstanceNamespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	after := testutil.ToFloat64(reconcilerActionsTotal.WithLabelValues(actionInSync))
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
