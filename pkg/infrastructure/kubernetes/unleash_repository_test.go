package kubernetes

import (
	"context"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = networkingv1.AddToScheme(scheme)
	_ = unleashv1.AddToScheme(scheme)
	return scheme
}

func newTestLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	return logger
}

func TestReconcileExtraIngresses_WithSecondaryClasses(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:        "unleash-ns",
			InstanceWebIngressHost:   "web.example.com",
			InstanceAPIIngressHost:   "api.example.com",
			InstanceWebIngressClass:  "primary-web",
			InstanceAPIIngressClass:  "primary-api",
			SecondaryWebIngressClass: "secondary-web",
			SecondaryAPIIngressClass: "secondary-api",
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.reconcileExtraIngresses(ctx, "my-instance")
	require.NoError(t, err)

	ingressList := &networkingv1.IngressList{}
	err = client.List(ctx, ingressList, &ctrl.ListOptions{Namespace: "unleash-ns"})
	require.NoError(t, err)
	assert.Len(t, ingressList.Items, 2, "expected 2 secondary ingresses (web + api)")

	webIngress := findIngress(ingressList.Items, "my-instance-web-secondary-web")
	require.NotNil(t, webIngress, "secondary web ingress should exist")
	assert.Equal(t, "secondary-web", *webIngress.Spec.IngressClassName)
	assert.Equal(t, "my-instance-web.example.com", webIngress.Spec.Rules[0].Host)
	assert.Equal(t, "true", webIngress.Labels["bifrost.nais.io/extra-ingress"])
	assert.Equal(t, "web", webIngress.Labels["bifrost.nais.io/ingress-type"])

	apiIngress := findIngress(ingressList.Items, "my-instance-api-secondary-api")
	require.NotNil(t, apiIngress, "secondary api ingress should exist")
	assert.Equal(t, "secondary-api", *apiIngress.Spec.IngressClassName)
	assert.Equal(t, "my-instance-api.example.com", apiIngress.Spec.Rules[0].Host)
	assert.Equal(t, "true", apiIngress.Labels["bifrost.nais.io/extra-ingress"])
	assert.Equal(t, "api", apiIngress.Labels["bifrost.nais.io/ingress-type"])
}

func TestReconcileExtraIngresses_WithoutSecondaryClasses(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:       "unleash-ns",
			InstanceWebIngressHost:  "web.example.com",
			InstanceAPIIngressHost:  "api.example.com",
			InstanceWebIngressClass: "primary-web",
			InstanceAPIIngressClass: "primary-api",
			// No secondary classes configured
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.reconcileExtraIngresses(ctx, "my-instance")
	require.NoError(t, err)

	ingressList := &networkingv1.IngressList{}
	err = client.List(ctx, ingressList, &ctrl.ListOptions{Namespace: "unleash-ns"})
	require.NoError(t, err)
	assert.Empty(t, ingressList.Items, "no secondary ingresses should be created")
}

func TestReconcileExtraIngresses_UpdatesExisting(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()

	oldClass := "old-class"
	existingIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-instance-web-secondary-web",
			Namespace: "unleash-ns",
			Labels: map[string]string{
				"bifrost.nais.io/extra-ingress": "true",
				"app.kubernetes.io/instance":    "my-instance",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &oldClass,
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingIngress).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:        "unleash-ns",
			InstanceWebIngressHost:   "web.example.com",
			InstanceAPIIngressHost:   "api.example.com",
			InstanceWebIngressClass:  "primary-web",
			InstanceAPIIngressClass:  "primary-api",
			SecondaryWebIngressClass: "secondary-web",
			// Only web secondary, no API secondary
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.reconcileExtraIngresses(ctx, "my-instance")
	require.NoError(t, err)

	updated := &networkingv1.Ingress{}
	err = client.Get(ctx, ctrl.ObjectKeyFromObject(existingIngress), updated)
	require.NoError(t, err)
	assert.Equal(t, "secondary-web", *updated.Spec.IngressClassName)
}

func TestDeleteExtraIngresses(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()

	webClass := "secondary-web"
	apiClass := "secondary-api"
	ingresses := []networkingv1.Ingress{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-instance-web-secondary-web",
				Namespace: "unleash-ns",
				Labels: map[string]string{
					"app.kubernetes.io/instance":    "my-instance",
					"bifrost.nais.io/extra-ingress": "true",
				},
			},
			Spec: networkingv1.IngressSpec{IngressClassName: &webClass},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-instance-api-secondary-api",
				Namespace: "unleash-ns",
				Labels: map[string]string{
					"app.kubernetes.io/instance":    "my-instance",
					"bifrost.nais.io/extra-ingress": "true",
				},
			},
			Spec: networkingv1.IngressSpec{IngressClassName: &apiClass},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-instance-web-secondary-web",
				Namespace: "unleash-ns",
				Labels: map[string]string{
					"app.kubernetes.io/instance":    "other-instance",
					"bifrost.nais.io/extra-ingress": "true",
				},
			},
			Spec: networkingv1.IngressSpec{IngressClassName: &webClass},
		},
	}

	objs := make([]ctrl.Object, len(ingresses))
	for i := range ingresses {
		objs[i] = &ingresses[i]
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace: "unleash-ns",
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.deleteExtraIngresses(ctx, "my-instance")
	require.NoError(t, err)

	ingressList := &networkingv1.IngressList{}
	err = client.List(ctx, ingressList, &ctrl.ListOptions{Namespace: "unleash-ns"})
	require.NoError(t, err)
	assert.Len(t, ingressList.Items, 1)
	assert.Equal(t, "other-instance-web-secondary-web", ingressList.Items[0].Name)
}

func TestBuildUnleashCRD_UsesPrimaryIngressClass(t *testing.T) {
	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:        "unleash-ns",
			InstanceWebIngressHost:   "web.example.com",
			InstanceAPIIngressHost:   "api.example.com",
			InstanceWebIngressClass:  "primary-web",
			InstanceAPIIngressClass:  "primary-api",
			SecondaryWebIngressClass: "secondary-web",
			SecondaryAPIIngressClass: "secondary-api",
			InstanceServiceaccount:   "sa",
			SQLInstanceID:            "sql-id",
			SQLInstanceRegion:        "europe-north1",
			SQLInstanceAddress:       "10.0.0.1",
			TeamsApiURL:              "https://console.example.com/graphql",
			TeamsApiSecretName:       "teams-secret",
			TeamsApiSecretTokenKey:   "token",
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

	// Primary class should be used in the CRD, not the secondary
	assert.Equal(t, "primary-web", crd.Spec.WebIngress.Class)
	assert.Equal(t, "primary-api", crd.Spec.ApiIngress.Class)

	// Hosts should be constructed correctly
	assert.Equal(t, "test-instance-web.example.com", crd.Spec.WebIngress.Host)
	assert.Equal(t, "test-instance-api.example.com", crd.Spec.ApiIngress.Host)
}

func findIngress(ingresses []networkingv1.Ingress, name string) *networkingv1.Ingress {
	for i := range ingresses {
		if ingresses[i].Name == name {
			return &ingresses[i]
		}
	}
	return nil
}

func TestReconcileAllExtraIngresses(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()

	// Create existing Unleash CRDs (simulating pre-existing instances)
	existingInstances := []unleashv1.Unleash{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "instance-a",
				Namespace: "unleash-ns",
			},
			Spec: unleashv1.UnleashSpec{
				WebIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "instance-a-web.example.com",
					Class:   "primary-web",
				},
				ApiIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "instance-a-api.example.com",
					Class:   "primary-api",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "instance-b",
				Namespace: "unleash-ns",
			},
			Spec: unleashv1.UnleashSpec{
				WebIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "instance-b-web.example.com",
					Class:   "primary-web",
				},
				ApiIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "instance-b-api.example.com",
					Class:   "primary-api",
				},
			},
		},
	}

	objs := make([]ctrl.Object, len(existingInstances))
	for i := range existingInstances {
		objs[i] = &existingInstances[i]
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:        "unleash-ns",
			InstanceWebIngressHost:   "web.example.com",
			InstanceAPIIngressHost:   "api.example.com",
			InstanceWebIngressClass:  "primary-web",
			InstanceAPIIngressClass:  "primary-api",
			SecondaryWebIngressClass: "secondary-web",
			SecondaryAPIIngressClass: "secondary-api",
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.ReconcileAllExtraIngresses(ctx)
	require.NoError(t, err)

	// Should have created 4 ingresses (2 instances × 2 secondary ingresses each)
	ingressList := &networkingv1.IngressList{}
	err = client.List(ctx, ingressList, &ctrl.ListOptions{Namespace: "unleash-ns"})
	require.NoError(t, err)
	assert.Len(t, ingressList.Items, 4)

	// Verify instance-a ingresses
	assert.NotNil(t, findIngress(ingressList.Items, "instance-a-web-secondary-web"))
	assert.NotNil(t, findIngress(ingressList.Items, "instance-a-api-secondary-api"))

	// Verify instance-b ingresses
	assert.NotNil(t, findIngress(ingressList.Items, "instance-b-web-secondary-web"))
	assert.NotNil(t, findIngress(ingressList.Items, "instance-b-api-secondary-api"))
}

func TestReconcileAllExtraIngresses_NoSecondaryClasses(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace: "unleash-ns",
		},
	}

	repo := &UnleashRepository{
		kubeClient: client,
		config:     cfg,
		logger:     newTestLogger(),
	}

	err := repo.ReconcileAllExtraIngresses(ctx)
	require.NoError(t, err)

	ingressList := &networkingv1.IngressList{}
	err = client.List(ctx, ingressList, &ctrl.ListOptions{Namespace: "unleash-ns"})
	require.NoError(t, err)
	assert.Empty(t, ingressList.Items)
}
