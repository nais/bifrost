//go:build kind_e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/bifrostclient"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sethvargo/go-envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResourceProfilesKind(t *testing.T) {
	// Refuse all cluster access unless the caller deliberately targets our local CI cluster.
	require.Equal(t, "1", os.Getenv("BIFROST_KIND_E2E"))
	require.Equal(t, "bifrost-profile-e2e", os.Getenv("BIFROST_E2E_NAMESPACE"))
	raw, err := clientcmd.LoadFromFile(os.Getenv("KUBECONFIG"))
	require.NoError(t, err)
	require.Equal(t, "kind-chart-testing", raw.CurrentContext)
	contextConfig := raw.Contexts[raw.CurrentContext]
	require.NotNil(t, contextConfig)
	cluster := raw.Clusters[contextConfig.Cluster]
	require.NotNil(t, cluster)
	endpoint, err := url.Parse(cluster.Server)
	require.NoError(t, err)
	require.Contains(t, []string{"127.0.0.1", "localhost"}, endpoint.Hostname())
	base, err := url.Parse(os.Getenv("BIFROST_E2E_URL"))
	require.NoError(t, err)
	require.Equal(t, "http", base.Scheme)
	require.Equal(t, "127.0.0.1", base.Hostname())
	key := os.Getenv("BIFROST_E2E_API_KEY")
	require.True(t, key != "", "test API key must be supplied via environment")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rest, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, nil, nil).ClientConfig()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, unleashv1.AddToScheme(scheme))
	kube, err := client.New(rest, client.Options{Scheme: scheme})
	require.NoError(t, err)
	deployment := &appsv1.Deployment{}
	require.NoError(t, kube.Get(ctx, client.ObjectKey{Namespace: "bifrost-profile-e2e-system", Name: "bifrost-profile-e2e-backend"}, deployment))
	for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
		if env.ValueFrom == nil {
			t.Setenv(env.Name, env.Value)
		}
	}
	cfg := &config.Config{}
	require.NoError(t, envconfig.Process(ctx, cfg))
	require.Equal(t, "bifrost-profile-e2e", cfg.Unleash.InstanceNamespace)
	require.True(t, cfg.Reconciler.Enabled)
	require.False(t, cfg.Reconciler.DryRun)
	require.False(t, cfg.Reconciler.AutoAdopt)
	require.True(t, cfg.Server.Auth.Enforced)
	require.Equal(t, time.Second, cfg.Reconciler.ResyncInterval)
	require.Contains(t, []string{"true", "false"}, os.Getenv("BIFROST_E2E_ENABLED"))
	enabled := os.Getenv("BIFROST_E2E_ENABLED") == "true"
	require.Equal(t, enabled, cfg.Unleash.ResourceProfilesEnabled)
	api, err := bifrostclient.NewClientWithResponses(base.String(), bifrostclient.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+key)
		return nil
	}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		response, err := http.Get(base.String() + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 10*time.Second, 100*time.Millisecond, "port-forwarded Bifrost must become reachable")
	unauthenticated, err := bifrostclient.NewClientWithResponses(base.String())
	require.NoError(t, err)
	denied, err := unauthenticated.GetInstanceWithResponse(ctx, "unauthenticated")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, denied.StatusCode(), "HTTP must enforce the chart-provided API key")

	for _, fixture := range []struct {
		name, nonce            string
		recorded, incompatible bool
	}{
		{name: "unannotated", nonce: "cilivenonce"},
		{name: "legacy-live-nonce", nonce: "cilivenonce", recorded: true},
		{name: "legacy-empty-nonce", recorded: true},
		{name: "incompatible-cpu-limit", nonce: "cilivenonce", recorded: true, incompatible: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			name := fixture.name + "-" + os.Getenv("BIFROST_E2E_ENABLED")
			intent, err := unleash.NewConfigBuilder().WithName(name).WithCustomVersion("5.1.0").WithFederation("renderonly", "ci-team", "", "").Build()
			require.NoError(t, err)
			live := kubernetes.BuildUnleashCRD(cfg, intent)
			live.Spec.Federation.SecretNonce = fixture.nonce
			live.Spec.Size = 4
			live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
			live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
			live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
			live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
			if fixture.incompatible {
				live.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("25m")
				live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("50m")
			}
			intent.FederationNonce = ""
			if fixture.recorded {
				if !enabled {
					intent.ResourceProfile = "large"
				}
				raw, err := kubernetes.MarshalIntent(intent)
				require.NoError(t, err)
				// Exercise pre-versioned legacy bytes as well as schema 1.
				if fixture.nonce == "" && enabled {
					raw = strings.Replace(raw, `"schemaVersion":1,`, "", 1)
				}
				live.Annotations[kubernetes.AnnotationDesiredState] = raw
			} else {
				delete(live.Annotations, kubernetes.AnnotationDesiredState)
				live.Spec.ExtraContainers[0].Args = []string{"unreadable-live-tuning"}
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: live.Namespace}, Data: map[string][]byte{"password": []byte("ci-only")}}
			require.NoError(t, kube.Create(ctx, secret))
			require.NoError(t, kube.Create(ctx, &live))
			read := func() *unleashv1.Unleash {
				t.Helper()
				cr := &unleashv1.Unleash{}
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(&live), cr))
				return cr
			}
			before := read()
			profile := bifrostclient.UnleashConfigRequestResourceProfileMedium
			response, err := api.UpdateInstanceWithResponse(ctx, name, bifrostclient.UpdateInstanceJSONRequestBody{ResourceProfile: &profile})
			require.NoError(t, err)
			after := read()
			if !fixture.recorded || !enabled || fixture.incompatible {
				require.Equal(t, http.StatusBadRequest, response.StatusCode(), "profile selection should be rejected")
				assert.Equal(t, before, after, "rejection must not write or adopt")
			} else {
				require.Equal(t, http.StatusOK, response.StatusCode(), "profile selection should succeed")
				want := before.DeepCopy()
				want.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
				want.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
				want.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
				assert.Equal(t, want.Spec, after.Spec, "no replica, sidecar, ingress, database or credential changes")
				assert.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
				intent.ResourceProfile = "medium"
				raw, err := kubernetes.MarshalIntent(intent)
				require.NoError(t, err)
				assert.Equal(t, raw, after.Annotations[kubernetes.AnnotationDesiredState])
				get, err := api.GetInstanceWithResponse(ctx, name)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, get.StatusCode())
				require.NotNil(t, get.JSON200)
				require.NotNil(t, get.JSON200.ResourceProfile)
				assert.Equal(t, bifrostclient.UnleashResourceProfileMedium, *get.JSON200.ResourceProfile)
			}
			if fixture.recorded {
				// A real controller repair proves writes are enabled, not just HTTP success.
				drifted := after.DeepCopy()
				drifted.Spec.ApiIngress.Class = "ci-drift"
				require.NoError(t, kube.Update(ctx, drifted))
				require.EventuallyWithT(t, func(c *assert.CollectT) {
					current := &unleashv1.Unleash{}
					if !assert.NoError(c, kube.Get(ctx, client.ObjectKeyFromObject(&live), current)) {
						return
					}
					assert.Equal(c, after.Spec, current.Spec)
					assert.Equal(c, after.Annotations, current.Annotations)
				}, 15*time.Second, 100*time.Millisecond)
				after = read()
			}
			// resyncInterval=1s in this dedicated release: observe multiple real resyncs.
			assert.Never(t, func() bool {
				current := &unleashv1.Unleash{}
				return kube.Get(ctx, client.ObjectKeyFromObject(&live), current) != nil || current.ResourceVersion != after.ResourceVersion
			}, 4*time.Second, 100*time.Millisecond, "converged and unannotated CRs must not be rewritten")
			assert.Equal(t, after, read())
			storedSecret := &corev1.Secret{}
			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(secret), storedSecret))
			assert.Equal(t, secret, storedSecret, "credential data and resourceVersion must remain unchanged")
			// No operator is installed: this proves CR writes, not Deployment Pod templates.
			var workloads appsv1.DeploymentList
			require.NoError(t, kube.List(ctx, &workloads, client.InNamespace(live.Namespace)))
			assert.Empty(t, workloads.Items)
		})
	}
}
