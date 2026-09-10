package reconciler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func legacyInstance(t *testing.T, name string) *unleashv1.Unleash {
	t.Helper()

	rendered := renderManaged(t, name)
	legacy := rendered.DeepCopy()
	legacy.Spec.CustomImage = ""
	legacy.Spec.ReleaseChannel.Name = "unleash-v6"
	legacy.Spec.Federation.Enabled = true
	legacy.Spec.Federation.SecretNonce = "legacy-nonce"
	delete(legacy.Annotations, kubernetes.AnnotationDesiredState)
	legacy.UID = types.UID(name + "-uid")
	legacy.Generation = 1
	return legacy
}

func adoptionReconciler(c client.Client, autoAdopt, dryRun bool) *UnleashReconciler {
	cfg := testConfig()
	return adoptionReconcilerForConfig(c, cfg, autoAdopt, dryRun)
}

func adoptionReconcilerForConfig(c client.Client, cfg *config.Config, autoAdopt, dryRun bool) *UnleashReconciler {
	cfg.Reconciler.AutoAdopt = autoAdopt
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	return NewUnleashReconciler(c, cfg, logger, time.Minute, dryRun)
}

func get(t *testing.T, c client.Client, namespace, name string) *unleashv1.Unleash {
	t.Helper()
	crd := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

func setCurrentCondition(crd *unleashv1.Unleash, condition string, status metav1.ConditionStatus) {
	meta.SetStatusCondition(&crd.Status.Conditions, metav1.Condition{
		Type: condition, Status: status, ObservedGeneration: crd.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func makeCurrentGenerationHealthy(t *testing.T, c client.Client, crd *unleashv1.Unleash) {
	t.Helper()
	setCurrentCondition(crd, unleashv1.UnleashStatusConditionTypeReconciled, metav1.ConditionTrue)
	setCurrentCondition(crd, unleashv1.UnleashStatusConditionTypeConnected, metav1.ConditionTrue)
	if err := c.Update(context.Background(), crd); err != nil {
		t.Fatal(err)
	}
}

func findEnv(crd *unleashv1.Unleash, name string) (corev1.EnvVar, bool) {
	for _, env := range crd.Spec.ExtraEnvVars {
		if env.Name == name {
			return env, true
		}
	}
	return corev1.EnvVar{}, false
}

func historicalDevNaisConfig() *config.Config {
	cfg := testConfig()
	cfg.Google.ProjectID = "nais-management-7178"
	cfg.Unleash.InstanceServiceaccount = "bifrost-unleash-sql-user"
	cfg.Unleash.SQLInstanceID = "bifrost-79ca6928"
	cfg.Unleash.SQLInstanceRegion = "europe-north1"
	cfg.Unleash.SQLInstanceAddress = "34.88.153.80"
	cfg.Unleash.InstanceWebIngressHost = "unleash-web.iap.dev-nais.cloud.nais.io"
	cfg.Unleash.InstanceWebIngressClass = "external-fa-haproxy"
	cfg.Unleash.InstanceAPIIngressHost = "unleash-api.dev-nais.cloud.nais.io"
	cfg.Unleash.InstanceAPIIngressClass = "internal-haproxy"
	cfg.Unleash.InstanceWebOAuthJWTAudience = "320980366213075636"
	return cfg
}

func historicalDevNaisV6(t *testing.T) *unleashv1.Unleash {
	t.Helper()

	intent, err := unleash.NewConfigBuilder().
		WithName("migration-test-v5").
		WithReleaseChannel("unleash-v6").
		WithFederation("kyi03r99", "", "unleasherator-federation-canary", "dev").
		Build()
	if err != nil {
		t.Fatal(err)
	}
	legacy := kubernetes.BuildUnleashCRD(historicalDevNaisConfig(), intent)
	delete(legacy.Annotations, kubernetes.AnnotationDesiredState)
	legacy.Labels["unleasherator.nais.io/federation-replay"] = "1785933988-3473-25"
	legacy.UID = types.UID("3bd4c7b5-bcf9-480c-a240-a77b8933d236")
	legacy.Generation = 6
	legacy.Spec.NetworkPolicy.ExtraEgressRules = legacy.Spec.NetworkPolicy.ExtraEgressRules[:1]
	legacy.Spec.ExtraEnvVars = []corev1.EnvVar{
		{Name: "OAUTH_JWT_AUDIENCE", Value: "320980366213075636"},
		{Name: "OAUTH_JWT_AUTH", Value: "true"},
		{Name: "TEAMS_API_URL", Value: "https://console.dev-nais.cloud.nais.io/graphql"},
		{
			Name: "TEAMS_API_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "teams-api-token"},
					Key:                  "token",
				},
			},
		},
		{Name: "TEAMS_ALLOWED_TEAMS"},
		{Name: "LOG_LEVEL", Value: "warn"},
		{Name: "DATABASE_POOL_MAX", Value: "3"},
		{Name: "DATABASE_POOL_IDLE_TIMEOUT_MS", Value: "1000"},
	}
	legacy.Status.Conditions = []metav1.Condition{
		{
			Type:               unleashv1.UnleashStatusConditionTypeReconciled,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: legacy.Generation,
		},
		{
			Type:   unleashv1.UnleashStatusConditionTypeConnected,
			Status: metav1.ConditionFalse,
		},
	}
	return &legacy
}

func TestLegacyAdoptionCanonicalizesHistoricalV6(t *testing.T) {
	crd := historicalDevNaisV6(t)
	c := newFakeClient(t, crd)

	adoptionReconcilerForConfig(c, historicalDevNaisConfig(), true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if live.Spec.ReleaseChannel.Name != "unleash-v6" || live.Spec.CustomImage != "" {
		t.Fatalf("version source = customImage %q, release channel %q; want release channel unleash-v6",
			live.Spec.CustomImage, live.Spec.ReleaseChannel.Name)
	}
	if !live.Spec.Federation.Enabled ||
		live.Spec.Federation.SecretNonce != "kyi03r99" ||
		len(live.Spec.Federation.Namespaces) != 1 ||
		live.Spec.Federation.Namespaces[0] != "unleasherator-federation-canary" ||
		len(live.Spec.Federation.Clusters) != 1 ||
		live.Spec.Federation.Clusters[0] != "dev" {
		t.Fatalf("federation identity was not preserved: %#v", live.Spec.Federation)
	}
	if len(live.Spec.NetworkPolicy.ExtraEgressRules) != 2 {
		t.Fatalf("network policy egress rules = %d, want Cloud SQL and NAIS API", len(live.Spec.NetworkPolicy.ExtraEgressRules))
	}
	if got := live.Spec.NetworkPolicy.ExtraEgressRules[0].To[0].IPBlock.CIDR; got != "34.88.153.80/32" {
		t.Fatalf("Cloud SQL network policy CIDR = %q, want 34.88.153.80/32", got)
	}
	if got := live.Spec.NetworkPolicy.ExtraEgressRules[1].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "nais-system" {
		t.Fatalf("NAIS API network policy namespace = %q, want nais-system", got)
	}
	if _, found := findEnv(live, "TEAMS_API_URL"); found {
		t.Fatal("legacy TEAMS_API_URL was retained")
	}
	if _, found := findEnv(live, "TEAMS_API_TOKEN"); found {
		t.Fatal("legacy TEAMS_API_TOKEN was retained")
	}
	naisAPIAddress, found := findEnv(live, "NAIS_API_ADDRESS")
	if !found || naisAPIAddress.Value != "nais-api.nais-system:3001" || naisAPIAddress.ValueFrom != nil {
		t.Fatalf("NAIS_API_ADDRESS = %#v, want current literal address", naisAPIAddress)
	}

	intent, present, err := recordedIntent(live)
	if err != nil || !present {
		t.Fatalf("desired-state intent = %#v, present=%t, err=%v", intent, present, err)
	}
	if intent.ReleaseChannelName != "unleash-v6" || !intent.EnableFederation ||
		intent.AllowedTeams != "" ||
		intent.AllowedNamespaces != "unleasherator-federation-canary" ||
		intent.AllowedClusters != "dev" {
		t.Fatalf("desired-state identity = %#v", intent)
	}
	if live.Labels[kubernetes.LabelManagedBy] != kubernetes.ManagedByBifrost {
		t.Fatal("canonical CR is missing Bifrost ownership label")
	}
	if live.Labels["unleasherator.nais.io/federation-replay"] != "1785933988-3473-25" {
		t.Fatal("unleasherator metadata was not preserved")
	}
	if live.Annotations[kubernetes.AnnotationAdoption] != adoptionPendingMarker {
		t.Fatalf("adoption marker = %q, want pending", live.Annotations[kubernetes.AnnotationAdoption])
	}
}

func TestLegacyAdoptionImmediatelyAdmitsNextCRAfterHealthyVerification(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := adoptionReconciler(c, true, false)

	r.adoptFleet(context.Background())
	if marker := get(t, c, first.Namespace, first.Name).Annotations[kubernetes.AnnotationAdoption]; marker != adoptionPendingMarker {
		t.Fatalf("first marker = %q, want pending", marker)
	}
	r.adoptFleet(context.Background())
	if _, present := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("adopted second CR while the first was pending")
	}

	makeCurrentGenerationHealthy(t, c, get(t, c, first.Namespace, first.Name))
	r.adoptFleet(context.Background())
	if _, present := get(t, c, first.Namespace, first.Name).Annotations[kubernetes.AnnotationAdoption]; present {
		t.Fatal("did not remove the healthy pending marker")
	}
	if marker := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationAdoption]; marker != adoptionPendingMarker {
		t.Fatalf("second marker = %q, want pending after the first CR was verified", marker)
	}
}

func TestLegacyAdoptionRejectsStaleReadiness(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := adoptionReconciler(c, true, false)
	r.adoptFleet(context.Background())

	live := get(t, c, first.Namespace, first.Name)
	makeCurrentGenerationHealthy(t, c, live)
	live = get(t, c, first.Namespace, first.Name)
	for i := range live.Status.Conditions {
		live.Status.Conditions[i].ObservedGeneration = live.Generation - 1
	}
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	r.adoptFleet(context.Background())
	if marker := get(t, c, first.Namespace, first.Name).Annotations[kubernetes.AnnotationAdoption]; marker != adoptionPendingMarker {
		t.Fatalf("stale-ready marker = %q, want pending", marker)
	}
	if _, present := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("stale readiness admitted the next CR")
	}
}

func TestLegacyAdoptionPendingMarkerSurvivesRestart(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)

	adoptionReconciler(c, true, false).adoptFleet(context.Background())
	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	if marker := get(t, c, first.Namespace, first.Name).Annotations[kubernetes.AnnotationAdoption]; marker != adoptionPendingMarker {
		t.Fatalf("pending marker = %q, want pending", marker)
	}
	if _, present := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("new reconciler bypassed pending health gate")
	}
}

func TestLegacyAdoptionAcceptsLatestCurrentGenerationWhilePending(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	c := newFakeClient(t, crd)
	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	updated := get(t, c, crd.Namespace, crd.Name)
	updated.Spec.Size = 2
	updated.Generation++
	if err := c.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	makeCurrentGenerationHealthy(t, c, get(t, c, crd.Namespace, crd.Name))

	adoptionReconciler(c, false, false).adoptFleet(context.Background())
	if _, present := get(t, c, crd.Namespace, crd.Name).Annotations[kubernetes.AnnotationAdoption]; present {
		t.Fatal("latest healthy generation did not clear the pending marker")
	}
}

func TestLegacyAdoptionCleansPendingMarkerAfterAutoAdoptIsDisabled(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	adoptionReconciler(c, true, false).adoptFleet(context.Background())
	makeCurrentGenerationHealthy(t, c, get(t, c, first.Namespace, first.Name))

	adoptionReconciler(c, false, false).adoptFleet(context.Background())

	if _, present := get(t, c, first.Namespace, first.Name).Annotations[kubernetes.AnnotationAdoption]; present {
		t.Fatal("autoAdopt=false did not clean up a healthy pending marker")
	}
	if _, present := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("autoAdopt=false admitted a new CR")
	}
}

func TestLegacyAdoptionDryRunRendersWithoutWriting(t *testing.T) {
	crd := legacyInstance(t, "team-preview")
	crd.Spec.Size = 4
	c := newFakeClient(t, crd)
	before, err := json.Marshal(get(t, c, crd.Namespace, crd.Name))
	if err != nil {
		t.Fatal(err)
	}

	adoptionReconciler(c, true, true).adoptFleet(context.Background())

	after, err := json.Marshal(get(t, c, crd.Namespace, crd.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("dry-run changed the legacy CR")
	}
}

func TestLegacyAdoptionSelectsPreviouslyLabelAdoptedCR(t *testing.T) {
	crd := legacyInstance(t, "team-label-only")
	c := newFakeClient(t, crd)

	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if _, present, err := recordedIntent(live); err != nil || !present {
		t.Fatalf("label-only CR desired state present=%t, err=%v", present, err)
	}
	if live.Annotations[kubernetes.AnnotationAdoption] != adoptionPendingMarker {
		t.Fatal("label-only CR was not admitted for adoption")
	}
}

func TestLegacyAdoptionPreservesObservedV7ReleaseChannel(t *testing.T) {
	crd := legacyInstance(t, "migration-test-v7")
	crd.Spec.ReleaseChannel.Name = "unleash-v7"
	c := newFakeClient(t, crd)

	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if live.Spec.ReleaseChannel.Name != "unleash-v7" {
		t.Fatalf("release channel = %q, want unleash-v7", live.Spec.ReleaseChannel.Name)
	}
	intent, present, err := recordedIntent(live)
	if err != nil || !present || intent.ReleaseChannelName != "unleash-v7" {
		t.Fatalf("desired-state release channel = %#v, present=%t, err=%v", intent, present, err)
	}
}

func TestLegacyAdoptionRefusesMalformedDesiredState(t *testing.T) {
	crd := legacyInstance(t, "team-malformed")
	crd.Annotations[kubernetes.AnnotationDesiredState] = `{"schemaVersion":99,"Name":"team-malformed"}`
	c := newFakeClient(t, crd)
	before, err := json.Marshal(get(t, c, crd.Namespace, crd.Name))
	if err != nil {
		t.Fatal(err)
	}

	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	after, err := json.Marshal(get(t, c, crd.Namespace, crd.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("malformed desired state was overwritten")
	}
	if got := seriesValue(t, "bifrost_reconciler_adoption_remaining", nil); got != 1 {
		t.Fatalf("adoption remaining = %v, want 1", got)
	}
}

func TestLegacyAdoptionDoesNotTouchForeignManagedCR(t *testing.T) {
	crd := legacyInstance(t, "team-foreign")
	delete(crd.Labels, kubernetes.LabelManagedBy)
	crd.Annotations[kubernetes.AnnotationAdoption] = adoptionPendingMarker
	c := newFakeClient(t, crd)

	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if live.Annotations[kubernetes.AnnotationAdoption] != adoptionPendingMarker {
		t.Fatal("foreign CR pending marker was changed")
	}
	if _, present := live.Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("foreign CR received Bifrost desired state")
	}
}

func TestLegacyAdoptionYieldsForChannelMigrationTransaction(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	crd.Annotations[kubernetes.AnnotationChannelMigration] = `{"phase":"target-written"}`
	c := newFakeClient(t, crd)

	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if _, present := live.Annotations[kubernetes.AnnotationDesiredState]; present {
		t.Fatal("legacy adoption changed a CR with a channel-migration transaction")
	}
	if live.Annotations[kubernetes.AnnotationAdoption] != "" {
		t.Fatal("legacy adoption set a pending marker while channel migration was active")
	}
}

func TestLegacyAdoptionWaitsForPendingDeletion(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	crd.Finalizers = []string{"unleash.nais.io/finalizer"}
	c := newFakeClient(t, crd)
	adoptionReconciler(c, true, false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if err := c.Delete(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	adoptionReconciler(c, false, false).adoptFleet(context.Background())
	if marker := get(t, c, crd.Namespace, crd.Name).Annotations[kubernetes.AnnotationAdoption]; marker != adoptionPendingMarker {
		t.Fatalf("deleting CR marker = %q, want pending", marker)
	}
}

func TestLegacyAdoptionStopsForUnsafeMarkers(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*unleashv1.Unleash, *unleashv1.Unleash)
	}{
		{
			name: "multiple pending",
			setup: func(first, second *unleashv1.Unleash) {
				first.Annotations[kubernetes.AnnotationAdoption] = adoptionPendingMarker
				second.Annotations[kubernetes.AnnotationAdoption] = adoptionPendingMarker
			},
		},
		{
			name: "unknown marker",
			setup: func(first, _ *unleashv1.Unleash) {
				first.Annotations[kubernetes.AnnotationAdoption] = "unexpected"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
			test.setup(first, second)
			c := newFakeClient(t, first, second)

			adoptionReconciler(c, true, false).adoptFleet(context.Background())

			if _, present := get(t, c, second.Namespace, second.Name).Annotations[kubernetes.AnnotationDesiredState]; present {
				t.Fatal("unsafe marker admitted a new CR")
			}
		})
	}
}
