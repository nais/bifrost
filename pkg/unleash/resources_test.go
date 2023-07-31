package unleash

import (
	"testing"

	fqdnV1alpha3 "github.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang/api/v1alpha3"
	"github.com/google/go-cmp/cmp"
	"github.com/nais/bifrost/pkg/config"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestNewFQDNNetworkPolicySpec(t *testing.T) {
	teamName := "my-team"
	kubeNamespace := "my-teamspace"

	protocolTCP := corev1.ProtocolTCP

	a := FQDNNetworkPolicySpec(teamName, kubeNamespace)
	b := fqdnV1alpha3.FQDNNetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "FQDNNetworkPolicy",
			APIVersion: "networking.gke.io/v1alpha3",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-team-fqdn",
			Namespace: kubeNamespace,
		},
		Spec: fqdnV1alpha3.FQDNNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance":   "my-team",
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
							FQDNs: []string{"sqladmin.googleapis.com", "www.gstatic.com"},
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
		t.Errorf(cmp.Diff(a, b))
	}
}

func TestNewUnleashSpec(t *testing.T) {
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
			TeamsApiURL:             "teams.example.com",
			TeamsApiSecretName:      "my-teams-api-secret",
			TeamsApiSecretTokenKey:  "token",
		},
		CloudConnectorProxy: "repo/connector:latest",
	}

	cloudSqlProto := corev1.ProtocolTCP
	cloudSqlPort := intstr.FromInt(3307)

	teamsApiProto := corev1.ProtocolTCP
	teamsApiPort := intstr.FromInt(3000)

	t.Run("default values", func(t *testing.T) {
		a := UnleashSpec(&c, "my-team", "", "", "", "")
		b := unleashv1.Unleash{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Unleash",
				APIVersion: "unleash.nais.io/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-team",
				Namespace: "unleash-ns",
			},
			Spec: unleashv1.UnleashSpec{
				Size: 1,
				Database: unleashv1.UnleashDatabaseConfig{
					Host:                  "localhost",
					Port:                  "5432",
					SSL:                   "false",
					SecretName:            "my-team",
					SecretUserKey:         "POSTGRES_USER",
					SecretPassKey:         "POSTGRES_PASSWORD",
					SecretDatabaseNameKey: "POSTGRES_DB",
				},
				WebIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "my-team-unleash-web.example.com",
					Path:    "/",
					Class:   "unleash-web-ingress-class",
				},
				ApiIngress: unleashv1.UnleashIngressConfig{
					Enabled: true,
					Host:    "my-team-unleash-api.example.com",
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
					}, {
						Ports: []networkingv1.NetworkPolicyPort{{
							Protocol: &teamsApiProto,
							Port:     &teamsApiPort,
						}},
						To: []networkingv1.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name=nais-system": "nais-system",
								},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/name": "teams-backend",
								},
							},
						}},
					}},
				},
				ExtraEnvVars: []corev1.EnvVar{{
					Name:  "GOOGLE_IAP_AUDIENCE",
					Value: "/projects/1234/global/backendServices/5678",
				}, {
					Name:  "TEAMS_API_URL",
					Value: "teams.example.com",
				}, {
					Name: "TEAMS_API_TOKEN",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "my-teams-api-secret",
							},
							Key: "token",
						},
					},
				}, {
					Name:  "TEAMS_ALLOWED_TEAMS",
					Value: "",
				}, {
					Name:  "TEAMS_ALLOWED_NAMESPACES",
					Value: "",
				}, {
					Name:  "TEAMS_ALLOWED_CLUSTERS",
					Value: "",
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
				}},
				ExistingServiceAccountName: "unleash-sa",
				Resources:                  corev1.ResourceRequirements{},
			},
		}

		assert.Equal(t, a.Spec, b.Spec)
	})

	t.Run("custom single values", func(t *testing.T) {
		a := UnleashSpec(&c, "my-team", "9.9.9", "my-team", "my-team-ns", "my-cluster")

		assert.Equal(t, a.Spec.CustomImage, "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:9.9.9")
		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_TEAMS",
			Value: "my-team",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_NAMESPACES",
			Value: "my-team-ns",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_CLUSTERS",
			Value: "my-cluster",
		})
	})

	t.Run("custom multiple values", func(t *testing.T) {
		a := UnleashSpec(&c, "my-team", "9.9.9", "team-a,team-b,team-c", "ns-a,ns-b,ns-c", "cluster-a,cluster-b,cluster-c")

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_TEAMS",
			Value: "team-a,team-b,team-c",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_NAMESPACES",
			Value: "ns-a,ns-b,ns-c",
		})

		assert.Contains(t, a.Spec.ExtraEnvVars, corev1.EnvVar{
			Name:  "TEAMS_ALLOWED_CLUSTERS",
			Value: "cluster-a,cluster-b,cluster-c",
		})
	})
}
