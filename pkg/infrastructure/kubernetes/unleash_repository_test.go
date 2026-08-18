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
