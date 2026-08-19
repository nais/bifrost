package kubernetes

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBuildUnleashCRD_UsesIngressClasses(t *testing.T) {
	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:       "unleash-ns",
			InstanceWebIngressHost:  "web.example.com",
			InstanceAPIIngressHost:  "api.example.com",
			InstanceWebIngressClass: "nais-ingress",
			InstanceAPIIngressClass: "nais-ingress-external",
			InstanceServiceaccount:  "sa",
			SQLInstanceID:           "sql-id",
			SQLInstanceRegion:       "europe-north1",
			SQLInstanceAddress:      "10.0.0.1",
			TeamsApiURL:             "https://console.example.com/graphql",
			TeamsApiSecretName:      "teams-secret",
			TeamsApiSecretTokenKey:  "token",
		},
		Google: config.GoogleConfig{
			ProjectID: "my-project",
		},
		CloudConnectorProxy: "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.1.0",
	}
	unleashCfg := &unleash.Config{
		Name:     "test-instance",
		LogLevel: "warn",
	}
	crd := BuildUnleashCRD(cfg, unleashCfg)
	// Configured ingress classes should be used directly in the CRD
	assert.Equal(t, "nais-ingress", crd.Spec.WebIngress.Class)
	assert.Equal(t, "nais-ingress-external", crd.Spec.ApiIngress.Class)
	// Hosts should be constructed correctly
	assert.Equal(t, "test-instance-web.example.com", crd.Spec.WebIngress.Host)
	assert.Equal(t, "test-instance-api.example.com", crd.Spec.ApiIngress.Host)
}

func TestReconcileIngressClasses_UpdatesStaleInstances(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, unleashv1.AddToScheme(scheme))

	stale := &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: "unleash-ns"},
		Spec: unleashv1.UnleashSpec{
			WebIngress: unleashv1.UnleashIngressConfig{Enabled: true, Class: "old-web"},
			ApiIngress: unleashv1.UnleashIngressConfig{Enabled: true, Class: "old-api"},
		},
	}
	upToDate := &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "unleash-ns"},
		Spec: unleashv1.UnleashSpec{
			WebIngress: unleashv1.UnleashIngressConfig{Enabled: true, Class: "external-fa-haproxy"},
			ApiIngress: unleashv1.UnleashIngressConfig{Enabled: true, Class: "internal-haproxy"},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, upToDate).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:       "unleash-ns",
			InstanceWebIngressClass: "external-fa-haproxy",
			InstanceAPIIngressClass: "internal-haproxy",
		},
	}

	repo := &UnleashRepository{kubeClient: client, config: cfg, logger: logrus.New()}

	require.NoError(t, repo.ReconcileIngressClasses(ctx))

	updated := &unleashv1.Unleash{}
	require.NoError(t, client.Get(ctx, ctrl.ObjectKeyFromObject(stale), updated))
	assert.Equal(t, "external-fa-haproxy", updated.Spec.WebIngress.Class)
	assert.Equal(t, "internal-haproxy", updated.Spec.ApiIngress.Class)
}

func repoTestConfig() *config.Config {
	return &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:      "unleash-ns",
			InstanceWebIngressHost: "web.example.com",
			InstanceAPIIngressHost: "api.example.com",
			InstanceServiceaccount: "sa",
			SQLInstanceID:          "sql-id",
			SQLInstanceRegion:      "europe-north1",
			SQLInstanceAddress:     "10.0.0.1",
			TeamsApiURL:            "https://console.example.com/graphql",
			TeamsApiSecretName:     "teams-secret",
			TeamsApiSecretTokenKey: "token",
		},
		Google:              config.GoogleConfig{ProjectID: "my-project"},
		CloudConnectorProxy: "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.1.0",
	}
}

func repoTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, unleashv1.AddToScheme(scheme))
	require.NoError(t, fqdnV1alpha3.AddToScheme(scheme))
	return scheme
}

// Create writes the network policy before the CRD. If the CRD write fails, the
// caller's rollback keys off the CRD having been created and so never touches
// the policy — leaving it orphaned and wedging every later retry on
// AlreadyExists. Create must clean up after itself.
func TestCreate_CleansUpNetworkPolicyWhenCRDFails(t *testing.T) {
	scheme := repoTestScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrl.WithWatch, obj ctrl.Object, opts ...ctrl.CreateOption) error {
				if _, ok := obj.(*unleashv1.Unleash); ok {
					return apierrors.NewInternalError(errors.New("crd write failed"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	repo := NewUnleashRepository(client, repoTestConfig(), logger)

	err := repo.Create(context.Background(), &unleash.Config{Name: "team-a"})
	require.Error(t, err, "CRD create failure must surface")

	policies := &fqdnV1alpha3.FQDNNetworkPolicyList{}
	require.NoError(t, client.List(context.Background(), policies))
	assert.Empty(t, policies.Items, "the network policy must not be left behind for the next attempt to trip over")
}

// A policy left behind by an earlier attempt, or by anything else, must not
// block a retry.
func TestCreate_ToleratesExistingNetworkPolicy(t *testing.T) {
	scheme := repoTestScheme(t)
	existing := &fqdnV1alpha3.FQDNNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-fqdn", Namespace: "unleash-ns"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	repo := NewUnleashRepository(client, repoTestConfig(), logger)

	require.NoError(t, repo.Create(context.Background(), &unleash.Config{Name: "team-a"}),
		"an existing network policy must not wedge the retry")

	created := &unleashv1.Unleash{}
	require.NoError(t, client.Get(context.Background(),
		ctrl.ObjectKey{Name: "team-a", Namespace: "unleash-ns"}, created))
}

// TEAMS_ALLOWED_TEAMS controls who may log in to the Unleash UI, independently
// of federation. LoadConfigFromCRD feeds the desired-state annotation, so
// losing it here does not just drop a value — it records the loss as
// authoritative intent, which the reconciler then enforces on every resync.
func TestLoadConfigFromCRD_KeepsAllowedTeamsWithoutFederation(t *testing.T) {
	for _, federated := range []bool{false, true} {
		name := "federation disabled"
		if federated {
			name = "federation enabled"
		}
		t.Run(name, func(t *testing.T) {
			crd := &unleashv1.Unleash{
				ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "unleash-ns"},
				Spec: unleashv1.UnleashSpec{
					ExtraEnvVars: []corev1.EnvVar{
						{Name: "TEAMS_ALLOWED_TEAMS", Value: "team-a,team-b"},
					},
					Federation: unleashv1.UnleashFederationConfig{Enabled: federated},
				},
			}

			cfg, err := LoadConfigFromCRD(crd).Build()
			require.NoError(t, err)
			assert.Equal(t, "team-a,team-b", cfg.AllowedTeams,
				"the allowed-team list must survive regardless of federation")
		})
	}
}

// Update renders the whole CRD from a config the caller assembled from an
// earlier read, so a write landing in between is overwritten wholesale — the
// resourceVersion copied from Update's own Get is younger than the data being
// written and protects nothing. The caller's resourceVersion turns that into a
// conflict it can surface or retry.
func TestUpdate_ExpectedResourceVersionDetectsConcurrentWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		// precondition picks what the caller passes, given the version it read.
		precondition func(read string) string
		wantConflict bool
	}{
		{
			name:         "the version the caller read is stale",
			precondition: func(read string) string { return read },
			wantConflict: true,
		},
		{
			name:         "no precondition still overwrites",
			precondition: func(string) string { return "" },
			wantConflict: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := repoTestScheme(t)
			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(
					&unleashv1.Unleash{
						ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "unleash-ns"},
						Spec:       unleashv1.UnleashSpec{CustomImage: "quay.io/unleash/unleash-server:5.1.2"},
					},
					&fqdnV1alpha3.FQDNNetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{Name: "team-a-fqdn", Namespace: "unleash-ns"},
					},
				).
				Build()

			logger := logrus.New()
			logger.SetOutput(io.Discard)
			repo := NewUnleashRepository(client, repoTestConfig(), logger)

			read, err := repo.Get(ctx, "team-a")
			require.NoError(t, err)
			require.NotEmpty(t, read.ResourceVersion, "the read must carry the version to write against")

			// Another writer — the migration reconciler in production — moves
			// the instance onto a release channel after the caller's read.
			concurrent := &unleashv1.Unleash{}
			require.NoError(t, client.Get(ctx, ctrl.ObjectKey{Name: "team-a", Namespace: "unleash-ns"}, concurrent))
			concurrent.Spec.CustomImage = ""
			concurrent.Spec.ReleaseChannel.Name = "stable-v6"
			require.NoError(t, client.Update(ctx, concurrent))

			err = repo.Update(ctx, &unleash.Config{Name: "team-a", CustomVersion: "5.1.2", LogLevel: "warn"},
				unleash.UpdateOptions{ExpectedResourceVersion: tc.precondition(read.ResourceVersion)})

			after := &unleashv1.Unleash{}
			require.NoError(t, client.Get(ctx, ctrl.ObjectKey{Name: "team-a", Namespace: "unleash-ns"}, after))

			if tc.wantConflict {
				require.Error(t, err, "a write against a superseded version must not be applied")
				assert.True(t, apierrors.IsConflict(err), "the conflict must survive wrapping so callers can map it to 409: %v", err)
				assert.Equal(t, "stable-v6", after.Spec.ReleaseChannel.Name, "the concurrent write must survive")
				return
			}

			require.NoError(t, err)
			assert.Empty(t, after.Spec.ReleaseChannel.Name, "without a precondition the concurrent write is lost, by design")
		})
	}
}

// Update renders a whole CRD and PUTs it, so anything the render does not
// produce is dropped from metadata unless it is carried over deliberately. That
// is not a cosmetic loss: unleasherator's finalizer is what makes it clean up
// after a deleted instance, and the federation canary marks approved smoke-test
// sources with labels of its own.
func TestUpdate_PreservesForeignMetadata(t *testing.T) {
	ctx := context.Background()
	scheme := repoTestScheme(t)

	live := &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-a",
			Namespace:  "unleash-ns",
			Finalizers: []string{"unleash.nais.io/finalizer"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "unleasherator.nais.io/v1",
				Kind:       "RemoteUnleash",
				Name:       "team-a-remote",
				UID:        "0d6a1f3e-6a2a-4f2b-9f4a-1f9a2b3c4d5e",
			}},
			Labels:      map[string]string{"unleasherator.nais.io/federation-smoke-test": "approved"},
			Annotations: map[string]string{"unleasherator.nais.io/federation-replay": "2026-08-18T09:00:00Z"},
		},
		Spec: unleashv1.UnleashSpec{CustomImage: "quay.io/unleash/unleash-server:5.1.2"},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(live, &fqdnV1alpha3.FQDNNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-fqdn", Namespace: "unleash-ns"},
		}).
		Build()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	repo := NewUnleashRepository(client, repoTestConfig(), logger)

	require.NoError(t, repo.Update(ctx, &unleash.Config{Name: "team-a", CustomVersion: "5.1.2", LogLevel: "warn"},
		unleash.UpdateOptions{}))

	after := &unleashv1.Unleash{}
	require.NoError(t, client.Get(ctx, ctrl.ObjectKey{Name: "team-a", Namespace: "unleash-ns"}, after))

	assert.Equal(t, []string{"unleash.nais.io/finalizer"}, after.Finalizers,
		"dropping the finalizer lets a later delete skip unleasherator's cleanup")
	if assert.Len(t, after.OwnerReferences, 1, "the owner must survive a PUT") {
		assert.Equal(t, "team-a-remote", after.OwnerReferences[0].Name)
	}
	assert.Equal(t, "approved", after.Labels["unleasherator.nais.io/federation-smoke-test"],
		"a foreign label marks state bifrost does not own and must not clear")
	assert.Equal(t, "2026-08-18T09:00:00Z", after.Annotations["unleasherator.nais.io/federation-replay"])

	// Bifrost's own two keys are still bifrost's to write.
	assert.Equal(t, ManagedByBifrost, after.Labels[LabelManagedBy])
	assert.NotEmpty(t, after.Annotations[AnnotationDesiredState])
}
