package unleash

import (
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/github"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestVersionFromImage(t *testing.T) {
	image := "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:1.2.3"
	expectedVersion := "1.2.3"

	version := versionFromImage(image)

	assert.Equal(t, expectedVersion, version)
}

func TestGetServerEnvVar(t *testing.T) {
	t.Run("should return value for existing variables", func(t *testing.T) {
		server := &unleashv1.Unleash{
			Spec: unleashv1.UnleashSpec{
				ExtraEnvVars: []corev1.EnvVar{
					{
						Name:  "MY_ENV_VAR",
						Value: "team-a,team-b",
					},
					{
						Name:  "MY_OTHER_ENV_VAR",
						Value: "namespace-a,namespace-b",
					},
				},
			},
		}

		assert.Equal(t, "team-a,team-b", getServerEnvVar(server, "MY_ENV_VAR", "default-value", true))
		assert.Equal(t, "default-value", getServerEnvVar(server, "NON_EXISTING_ENV_VAR", "default-value", true))
	})

	t.Run("should return default value for non-existing variables", func(t *testing.T) {
		server := &unleashv1.Unleash{
			Spec: unleashv1.UnleashSpec{
				ExtraEnvVars: []corev1.EnvVar{
					{
						Name:  "TEAMS_ALLOWED_TEAMS",
						Value: "team-a,team-b",
					},
				},
			},
		}

		assert.Equal(t, "default-value", getServerEnvVar(server, "NON_EXISTING_ENV_VAR", "default-value", true))
	})

	t.Run("should return empty string for non-existing variables when default value is disabled", func(t *testing.T) {
		server := &unleashv1.Unleash{
			Spec: unleashv1.UnleashSpec{
				ExtraEnvVars: []corev1.EnvVar{
					{
						Name:  "TEAMS_ALLOWED_TEAMS",
						Value: "team-a,team-b",
					},
				},
			},
		}

		assert.Equal(t, "", getServerEnvVar(server, "NON_EXISTING_ENV_VAR", "default-value", false))
	})
}

func TestCustomImageForVersion(t *testing.T) {
	customVersion := "1.2.3"
	expectedImage := "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:1.2.3"

	assert.Equal(t, expectedImage, customImageForVersion(customVersion))
}

func TestUnleashVariables(t *testing.T) {
	c := &config.Config{}

	unleashInstance := UnleashDefinition(c, &UnleashConfig{
		Name:                      "my-instance",
		CustomVersion:             "1.2.3",
		EnableFederation:          true,
		FederationNonce:           "abc123",
		AllowedTeams:              "team-a,team-b",
		AllowedNamespaces:         "namespace-a,namespace-b",
		AllowedClusters:           "cluster-a,cluster-b",
		LogLevel:                  "debug",
		DatabasePoolMax:           10,
		DatabasePoolIdleTimeoutMs: 100,
	})
	uc := *UnleashVariables(&unleashInstance, true)
	assert.Equal(t, UnleashConfig{
		Name:                      "my-instance",
		CustomVersion:             "1.2.3",
		EnableFederation:          true,
		FederationNonce:           "abc123",
		AllowedTeams:              "team-a,team-b",
		AllowedNamespaces:         "namespace-a,namespace-b",
		AllowedClusters:           "cluster-a,cluster-b",
		LogLevel:                  "debug",
		DatabasePoolMax:           10,
		DatabasePoolIdleTimeoutMs: 100,
	}, uc)

	unleashInstance = unleashv1.Unleash{}
	uc = *UnleashVariables(&unleashInstance, true)
	assert.Equal(t, UnleashConfig{
		LogLevel:                  "warn",
		DatabasePoolMax:           3,
		DatabasePoolIdleTimeoutMs: 1000,
	}, uc)
}

func TestFQDNNetworkPolicySpec(t *testing.T) {
	teamName := "my-instance"
	kubeNamespace := "my-instancespace"

	protocolTCP := corev1.ProtocolTCP

	a := FQDNNetworkPolicyDefinition(teamName, kubeNamespace)
	b := fqdnV1alpha3.FQDNNetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "FQDNNetworkPolicy",
			APIVersion: "networking.gke.io/v1alpha3",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-instance-fqdn",
			Namespace: kubeNamespace,
		},
		Spec: fqdnV1alpha3.FQDNNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance":   "my-instance",
					"app.kubernetes.io/part-of":    "unleasherator",
					"app.kubernetes.io/name":       "Unleash",
					"app.kubernetes.io/created-by": "controller-manager",
				},
			},
			Egress: []fqdnV1alpha3.FQDNNetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 443},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{"sqladmin.googleapis.com", "www.gstatic.com", "hooks.slack.com", "console.nav.cloud.nais.io", "auth.nais.io"},
						},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 80},
							Protocol: &protocolTCP,
						},
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 988},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{"metadata.google.internal"},
						},
					},
				},
			},
		},
	}

	if !cmp.Equal(a, b) {
		t.Error(cmp.Diff(a, b))
	}
}

func TestUnleashSpec(t *testing.T) {
	c := config.Config{
		Google: config.GoogleConfig{
			ProjectID:           "my-project",
			ProjectNumber:       "1234",
			IAPBackendServiceID: "5678",
		},
		Unleash: config.UnleashConfig{
			InstanceNamespace:       "unleash-ns",
			InstanceServiceaccount:  "unleash-sa",
			SQLInstanceID:           "my-instance",
			SQLInstanceRegion:       "my-region",
			SQLInstanceAddress:      "1.2.3.4",
			InstanceWebIngressHost:  "unleash-web.example.com",
			InstanceWebIngressClass: "unleash-web-ingress-class",
			InstanceAPIIngressHost:  "unleash-api.example.com",
			InstanceAPIIngressClass: "unleash-api-ingress-class",
			TeamsApiURL:             "https://teams.example.com/query",
			TeamsApiSecretName:      "my-instances-api-secret",
			TeamsApiSecretTokenKey:  "token",
		},
		CloudConnectorProxy: "repo/connector:latest",
	}

	cloudSqlProto := corev1.ProtocolTCP
	cloudSqlPort := intstr.FromInt(3307)

	t.Run("default values", func(t *testing.T) {
		a := UnleashDefinition(&c, &UnleashConfig{Name: "my-instance", FederationNonce: "my-nonce"})
		b := unleashv1.Unleash{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Unleash",
				APIVersion: "unleash.nais.io/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-instance",
				Namespace: "unleash-ns",
			},
			Spec: unleashv1.UnleashSpec{
				Size: 1,
				Database: unleashv1.UnleashDatabaseConfig{
					Host:                  "localhost",
					Port:                  "5432",
					SSL:                   "false",
					SecretName:            "my-instance",
					SecretUserKey:         "POSTGRES_USER",
					SecretPassKey:         "POSTGRES_PASSWORD",
					SecretDatabaseNameKey: "POSTGRES_DB",
				},
				WebIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "my-instance-unleash-web.example.com",
					Path:    "/",
					Class:   "unleash-web-ingress-class",
				},
				ApiIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "my-instance-unleash-api.example.com",
					Path:    "/",
					Class:   "unleash-api-ingress-class",
				},
				NetworkPolicy: unleashv1.UnleashNetworkPolicyConfig{
					Enabled:  true,
					AllowDNS: true,
					ExtraEgressRules: []networkingv1.NetworkPolicyEgressRule{{
						Ports: []networkingv1.NetworkPolicyPort{{
							Protocol: &cloudSqlProto,
							Port:     &cloudSqlPort,
						}},
						To: []networkingv1.NetworkPolicyPeer{{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "1.2.3.4/32",
							},
						}},
					}},
				},
				Federation: unleashv1.UnleashFederationConfig{
					Enabled:     false,
					Clusters:    []string{},
					Namespaces:  []string{},
					SecretNonce: "my-nonce",
				},
				ExtraEnvVars: []corev1.EnvVar{{
					Name:  "GOOGLE_IAP_AUDIENCE",
					Value: "/projects/1234/global/backendServices/5678",
				}, {
					Name:  "TEAMS_API_URL",
					Value: "https://teams.example.com/query",
				}, {
					Name: "TEAMS_API_TOKEN",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "my-instances-api-secret",
							},
							Key: "token",
						},
					},
				}, {
					Name:  "TEAMS_ALLOWED_TEAMS",
					Value: "",
				}, {
					Name:  "LOG_LEVEL",
					Value: "",
				}, {
					Name:  "DATABASE_POOL_MAX",
					Value: "0",
				}, {
					Name:  "DATABASE_POOL_IDLE_TIMEOUT_MS",
					Value: "0",
				}},
				ExtraContainers: []corev1.Container{{
					Name:  "sql-proxy",
					Image: "repo/connector:latest",
					Args: []string{
						"--structured-logs",
						"--port=5432",
						"my-project:my-region:my-instance",
					},
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
						Privileged:               boolRef(false),
						RunAsUser:                int64Ref(65532),
						RunAsNonRoot:             boolRef(true),
						AllowPrivilegeEscalation: boolRef(false),
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("10m"),
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
				}},
				ExistingServiceAccountName: "unleash-sa",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		}

		assert.Equal(t, a.Spec, b.Spec)
	})

	t.Run("custom single values", func(t *testing.T) {
		uc := &UnleashConfig{
			Name:                      "my-instance",
			CustomVersion:             "9.9.9",
			EnableFederation:          true,
			AllowedTeams:              "my-team",
			AllowedNamespaces:         "my-namespace",
			AllowedClusters:           "my-cluster",
			LogLevel:                  "debug",
			DatabasePoolMax:           10,
			DatabasePoolIdleTimeoutMs: 100,
		}
		a := UnleashDefinition(&c, uc)

		assert.Equal(t, a.Spec.CustomImage, "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:9.9.9")
		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_TEAMS",
			Value: "my-team",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "LOG_LEVEL",
			Value: "debug",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "DATABASE_POOL_MAX",
			Value: "10",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "DATABASE_POOL_IDLE_TIMEOUT_MS",
			Value: "100",
		})
	})

	t.Run("custom multiple values", func(t *testing.T) {
		uc := &UnleashConfig{
			Name:                      "my-instance",
			CustomVersion:             "9.9.9",
			AllowedTeams:              "team-a,team-b,team-c",
			AllowedNamespaces:         "ns-a,ns-b,ns-c",
			AllowedClusters:           "cluster-a,cluster-b,cluster-c",
			LogLevel:                  "debug",
			DatabasePoolMax:           10,
			DatabasePoolIdleTimeoutMs: 100,
		}

		a := UnleashDefinition(&c, uc)

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_TEAMS",
			Value: "team-a,team-b,team-c",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "LOG_LEVEL",
			Value: "debug",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "DATABASE_POOL_MAX",
			Value: "10",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "DATABASE_POOL_IDLE_TIMEOUT_MS",
			Value: "100",
		})
	})
}

func TestMergeTeamsAndNamespaces(t *testing.T) {
	testCases := []struct {
		name               string
		allowedTeams       string
		allowedNamespaces  string
		expectedTeams      string
		expectedNamespaces string
	}{
		{
			name:               "Merge teams and namespaces",
			allowedTeams:       "team-a,team-b",
			allowedNamespaces:  "namespace-a,namespace-b",
			expectedTeams:      "namespace-a,namespace-b,team-a,team-b",
			expectedNamespaces: "namespace-a,namespace-b,team-a,team-b",
		},
		{
			name:               "Empty teams and namespaces",
			allowedTeams:       "",
			allowedNamespaces:  "",
			expectedTeams:      "",
			expectedNamespaces: "",
		},
		{
			name:               "Teams and namespaces with leading/trailing spaces",
			allowedTeams:       " team-a , team-b ",
			allowedNamespaces:  " namespace-a , namespace-b ",
			expectedTeams:      "namespace-a,namespace-b,team-a,team-b",
			expectedNamespaces: "namespace-a,namespace-b,team-a,team-b",
		},
		{
			name:               "Teams and namespaces with duplicate values",
			allowedTeams:       "team-a,team-a,team-b,team-b",
			allowedNamespaces:  "namespace-a,namespace-a,namespace-b,namespace-b",
			expectedTeams:      "namespace-a,namespace-b,team-a,team-b",
			expectedNamespaces: "namespace-a,namespace-b,team-a,team-b",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			uc := &UnleashConfig{
				AllowedTeams:      tc.allowedTeams,
				AllowedNamespaces: tc.allowedNamespaces,
			}

			uc.MergeTeamsAndNamespaces()

			assert.Equal(t, tc.expectedTeams, uc.AllowedTeams)
			assert.Equal(t, tc.expectedNamespaces, uc.AllowedNamespaces)
		})
	}
}

func TestSetDefaultValues(t *testing.T) {
	uc := &UnleashConfig{}

	uc.SetDefaultValues([]github.UnleashVersion{{
		GitTag: "v5.10.2-20240329-070801-0180a96",
	}})

	assert.Equal(t, LogLevel, uc.LogLevel)
	assert.Equal(t, DatabasePoolMax, strconv.Itoa(uc.DatabasePoolMax))
	assert.Equal(t, DatabasePoolIdleTimeoutMs, strconv.Itoa(uc.DatabasePoolIdleTimeoutMs))
	assert.Equal(t, "v5.10.2-20240329-070801-0180a96", uc.CustomVersion)
}
