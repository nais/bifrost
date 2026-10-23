package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	appunleash "github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/bifrostclient"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/nais/bifrost/pkg/reconciler"
	fqdn "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	kubeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestProfileAPIWritesAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		body    string
		enabled bool
		status  int
		profile string
	}{
		{`{"resource_profile":"medium"}`, true, http.StatusOK, "medium"},
		{`{"resource_profile":"small"}`, true, http.StatusOK, "small"},
		{`{"log_level":"debug"}`, true, http.StatusOK, ""},
		{`{"resource_profile":null}`, true, http.StatusBadRequest, ""},
		{`{"resource_profile":""}`, true, http.StatusBadRequest, ""},
		{`{"resource_profile":"huge"}`, true, http.StatusBadRequest, ""},
		{`{"resource_profile":"large"}`, false, http.StatusBadRequest, ""},
	} {
		t.Run(tc.body+"/enabled="+map[bool]string{true: "true", false: "false"}[tc.enabled], func(t *testing.T) {
			settings := &config.Config{Unleash: config.UnleashConfig{InstanceNamespace: "unleash", ResourceProfilesEnabled: tc.enabled}}
			cfg, err := unleash.NewConfigBuilder().WithName("team-a").WithCustomVersion("5.1.0").WithLogLevel("error").WithDatabasePool(7, 900).WithFederation("fixed-nonce", "team-a", "", "").Build()
			require.NoError(t, err)
			live := kubernetes.BuildUnleashCRD(settings, cfg)
			live.Spec.Size = 4
			live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
			live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
			live.Spec.ExtraContainers[0].Args = []string{"tuned-proxy"}
			scheme := runtime.NewScheme()
			require.NoError(t, unleashv1.AddToScheme(scheme))
			require.NoError(t, fqdn.AddToScheme(scheme))
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&live, &fqdn.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: live.Name + "-fqdn", Namespace: live.Namespace}}).Build()
			repo := kubernetes.NewUnleashRepository(client, settings, logrus.New())
			before, err := repo.GetCRD(context.Background(), live.Name)
			require.NoError(t, err)
			service := appunleash.NewService(repo, &MockDatabaseManager{}, settings, logrus.New())
			handler := NewUnleashHandler(service, settings, logrus.New(), nil)
			router := gin.New()
			router.PUT("/unleash/:name", handler.UpdateInstance)
			req := httptest.NewRequest(http.MethodPut, "/unleash/team-a", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			after, err := repo.GetCRD(context.Background(), live.Name)
			require.NoError(t, err)
			if tc.status != http.StatusOK {
				assert.Equal(t, before, after)
				return
			}
			want := before.DeepCopy()
			if tc.profile != "" {
				kubernetes.MergeResourceOwnership(before, want, tc.profile, true)
				assert.Equal(t, want.Spec, after.Spec, "profile-only HTTP updates must preserve the whole unrelated spec")
			} else {
				assert.Equal(t, before.Spec.Resources, after.Spec.Resources)
			}
			intent, err := kubernetes.UnmarshalIntent(after.Annotations[kubernetes.AnnotationDesiredState])
			require.NoError(t, err)
			assert.Equal(t, tc.profile, intent.ResourceProfile)
		})
	}
}

type forbiddenDatabaseOperations struct{ t *testing.T }

func (db forbiddenDatabaseOperations) unexpected() error {
	db.t.Helper()
	db.t.Error("profile update attempted database or credential lifecycle operations")
	return fmt.Errorf("database lifecycle operations forbidden during profile updates")
}

func (db forbiddenDatabaseOperations) CreateDatabase(context.Context, string) (bool, error) {
	return false, db.unexpected()
}

func (db forbiddenDatabaseOperations) CreateDatabaseUser(context.Context, string) (string, bool, error) {
	return "", false, db.unexpected()
}

func (db forbiddenDatabaseOperations) CreateSecret(context.Context, string, string) (bool, error) {
	return false, db.unexpected()
}

func (db forbiddenDatabaseOperations) DeleteDatabase(context.Context, string) error {
	return db.unexpected()
}

func (db forbiddenDatabaseOperations) DeleteDatabaseUser(context.Context, string) error {
	return db.unexpected()
}

func (db forbiddenDatabaseOperations) DeleteSecret(context.Context, string) error {
	return db.unexpected()
}

func TestProfileSelectionRequiresIntentAndPreservesLegacyNonceThroughReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body, liveNonce        string
		recorded, disabled, conflict bool
	}{
		{name: "unannotated-profile-only", body: `{"resource_profile":"medium"}`, liveNonce: "live-nonce"},
		{name: "unannotated-mixed-update", body: `{"resource_profile":"medium","log_level":"debug"}`, liveNonce: "live-nonce"},
		{name: "empty-intent-nonce", body: `{"resource_profile":"medium"}`, liveNonce: "live-nonce", recorded: true},
		{name: "both-nonces-empty", body: `{"resource_profile":"medium"}`, recorded: true},
		{name: "disabled", body: `{"resource_profile":"medium"}`, liveNonce: "live-nonce", recorded: true, disabled: true},
		{name: "optimistic-conflict", body: `{"resource_profile":"medium"}`, liveNonce: "live-nonce", recorded: true, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			settings := &config.Config{CloudConnectorProxy: "example.com/proxy:current", Unleash: config.UnleashConfig{
				InstanceNamespace: "unleash", ResourceProfilesEnabled: !tc.disabled,
				SQLInstanceID: "shared-sql", SQLInstanceRegion: "europe-north1", SQLInstanceAddress: "10.0.0.1",
				InstanceServiceaccount: "sql-user", InstanceWebIngressHost: "web.example", InstanceAPIIngressHost: "api.example",
			}}
			cfg, err := unleash.NewConfigBuilder().WithName("team-a").WithCustomVersion("5.1.0").WithFederation("render-nonce", "team-a", "", "").Build()
			require.NoError(t, err)
			live := kubernetes.BuildUnleashCRD(settings, cfg)
			live.Spec.Federation.SecretNonce = tc.liveNonce
			live.Spec.Size = 4
			live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
			live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
			live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
			cfg.FederationNonce = ""
			if tc.recorded {
				raw, err := kubernetes.MarshalIntent(cfg)
				require.NoError(t, err)
				live.Annotations[kubernetes.AnnotationDesiredState] = raw
			} else {
				delete(live.Annotations, kubernetes.AnnotationDesiredState)
				live.Spec.ExtraContainers[0].Args = []string{"unreadable-live-tuning"}
			}
			scheme := runtime.NewScheme()
			require.NoError(t, unleashv1.AddToScheme(scheme))
			require.NoError(t, fqdn.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: live.Namespace}, Data: map[string][]byte{"password": []byte("unchanged-test-value")}}
			concurrent := make(chan *unleashv1.Unleash, 1)
			var updateAttempts atomic.Int32
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&live, secret).
				WithInterceptorFuncs(interceptor.Funcs{Update: func(ctx context.Context, c kubeclient.WithWatch, obj kubeclient.Object, opts ...kubeclient.UpdateOption) error {
					attempt := updateAttempts.Add(1)
					if tc.conflict && attempt == 1 {
						winner := &unleashv1.Unleash{}
						if err := c.Get(ctx, kubeclient.ObjectKeyFromObject(obj), winner); err != nil {
							return err
						}
						winner.Annotations["test/concurrent-writer"] = "retained"
						if err := c.Update(ctx, winner); err != nil {
							return err
						}
						concurrent <- winner.DeepCopy()
					}
					return c.Update(ctx, obj, opts...)
				}}).Build()
			logger := logrus.New()
			repo := kubernetes.NewUnleashRepository(client, settings, logger)
			before, err := repo.GetCRD(ctx, live.Name)
			require.NoError(t, err)
			beforeSecret := &corev1.Secret{}
			require.NoError(t, client.Get(ctx, kubeclient.ObjectKeyFromObject(secret), beforeSecret))
			service := appunleash.NewService(repo, forbiddenDatabaseOperations{t}, settings, logger)
			router := gin.New()
			router.PUT("/v1/unleash/:name", NewUnleashHandler(service, settings, logger, nil).UpdateInstance)
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			api, err := bifrostclient.NewClientWithResponses(server.URL)
			require.NoError(t, err)
			var body bifrostclient.UpdateInstanceJSONRequestBody
			require.NoError(t, json.Unmarshal([]byte(tc.body), &body))
			response, err := api.UpdateInstanceWithResponse(ctx, live.Name, body)
			require.NoError(t, err)
			after, err := repo.GetCRD(ctx, live.Name)
			require.NoError(t, err)
			if tc.conflict {
				require.Equal(t, http.StatusConflict, response.StatusCode(), string(response.Body))
				require.Len(t, concurrent, 1)
				assert.Equal(t, <-concurrent, after, "conflicting request must not overwrite the concurrent writer")
				assert.Equal(t, before.Spec, after.Spec)
				assert.Equal(t, before.Annotations[kubernetes.AnnotationDesiredState], after.Annotations[kubernetes.AnnotationDesiredState])
				assert.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
				assert.EqualValues(t, 1, updateAttempts.Load())
			} else if tc.recorded && !tc.disabled {
				require.Equal(t, http.StatusOK, response.StatusCode(), string(response.Body))
				require.NotNil(t, response.JSON200)
				require.NotNil(t, response.JSON200.ResourceProfile)
				assert.Equal(t, bifrostclient.UnleashResourceProfileMedium, *response.JSON200.ResourceProfile)
				require.NotNil(t, response.JSON200.ResourceProfileExplicit)
				assert.True(t, *response.JSON200.ResourceProfileExplicit)
				require.NotNil(t, response.JSON200.ResourceProfileMatches)
				assert.True(t, *response.JSON200.ResourceProfileMatches)
				require.NotNil(t, response.JSON200.ResolvedResources)
				require.NotNil(t, response.JSON200.ResolvedResources.Requests)
				assert.Equal(t, "256Mi", (*response.JSON200.ResolvedResources.Requests)["memory"])
				assert.Equal(t, "100m", (*response.JSON200.ResolvedResources.Requests)["cpu"])
				require.NotNil(t, response.JSON200.ResolvedResources.Limits)
				assert.Equal(t, "512Mi", (*response.JSON200.ResolvedResources.Limits)["memory"])
				want := before.DeepCopy()
				kubernetes.MergeResourceOwnership(before, want, "medium", true)
				assert.Equal(t, want.Spec, after.Spec, "only selected resource keys may change, never credentials or database references")
				assert.Equal(t, resource.MustParse("100m"), after.Spec.Resources.Requests[corev1.ResourceCPU])
				assert.Equal(t, resource.MustParse("256Mi"), after.Spec.Resources.Requests[corev1.ResourceMemory])
				assert.Equal(t, resource.MustParse("512Mi"), after.Spec.Resources.Limits[corev1.ResourceMemory])
				assert.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
				assert.EqualValues(t, 1, updateAttempts.Load())
				cfg.ResourceProfile = "medium"
				raw, err := kubernetes.MarshalIntent(cfg)
				require.NoError(t, err)
				assert.Equal(t, raw, after.Annotations[kubernetes.AnnotationDesiredState], "profile selection must not fill unspecified intent fields")
			} else {
				require.Equal(t, http.StatusBadRequest, response.StatusCode(), string(response.Body))
				if tc.disabled {
					assert.Contains(t, string(response.Body), "resource_profiles_disabled")
				} else {
					assert.Contains(t, string(response.Body), "recorded_intent_required")
				}
				assert.Equal(t, before, after, "rejected profile selection must not adopt or write")
				assert.Zero(t, updateAttempts.Load())
			}
			assert.Equal(t, before.Spec.Size, after.Spec.Size)
			assert.Equal(t, before.Spec.ExtraContainers, after.Spec.ExtraContainers)
			assert.Equal(t, before.Spec.Database, after.Spec.Database)
			assert.Equal(t, before.Spec.WebIngress, after.Spec.WebIngress)
			assert.Equal(t, before.Spec.ApiIngress, after.Spec.ApiIngress)
			assert.Equal(t, before.Spec.ExistingServiceAccountName, after.Spec.ExistingServiceAccountName)
			r := reconciler.NewUnleashReconciler(client, settings, logger, time.Minute, false)
			for range 2 {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: live.Name, Namespace: live.Namespace}})
				require.NoError(t, err)
				current, err := repo.GetCRD(ctx, live.Name)
				require.NoError(t, err)
				assert.Equal(t, after, current, "next reconciles must not rotate credentials, rewrite annotations or apply lossy readback")
			}
			storedSecret := &corev1.Secret{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, storedSecret))
			assert.Equal(t, beforeSecret, storedSecret, "credential data and resourceVersion must remain unchanged")
		})
	}
}
