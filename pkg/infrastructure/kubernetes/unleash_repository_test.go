package kubernetes

import (
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/stretchr/testify/assert"
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
