package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	appunleash "github.com/nais/bifrost/pkg/application/unleash"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestProfileSelectionRequiresIntentAndPreservesLegacyNonceThroughReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body, liveNonce string
		recorded              bool
	}{
		{"unannotated-profile-only", `{"resource_profile":"medium"}`, "live-nonce", false},
		{"unannotated-mixed-update", `{"resource_profile":"medium","log_level":"debug"}`, "live-nonce", false},
		{"empty-intent-nonce", `{"resource_profile":"medium"}`, "live-nonce", true},
		{"both-nonces-empty", `{"resource_profile":"medium"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			settings := &config.Config{Unleash: config.UnleashConfig{InstanceNamespace: "unleash", ResourceProfilesEnabled: true}}
			cfg, err := unleash.NewConfigBuilder().WithName("team-a").WithCustomVersion("5.1.0").WithFederation("render-nonce", "team-a", "", "").Build()
			require.NoError(t, err)
			live := kubernetes.BuildUnleashCRD(settings, cfg)
			live.Spec.Federation.SecretNonce = tc.liveNonce
			live.Spec.Size = 4
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
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&live, secret).Build()
			logger := logrus.New()
			repo := kubernetes.NewUnleashRepository(client, settings, logger)
			before, err := repo.GetCRD(ctx, live.Name)
			require.NoError(t, err)
			service := appunleash.NewService(repo, &MockDatabaseManager{}, settings, logger)
			router := gin.New()
			router.PUT("/unleash/:name", NewUnleashHandler(service, settings, logger, nil).UpdateInstance)
			request := httptest.NewRequest(http.MethodPut, "/unleash/team-a", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			after, err := repo.GetCRD(ctx, live.Name)
			require.NoError(t, err)
			if tc.recorded {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				want := before.DeepCopy()
				kubernetes.MergeResourceOwnership(before, want, "medium", true)
				assert.Equal(t, want.Spec, after.Spec, "only selected resource keys may change, never credentials or database references")
				cfg.ResourceProfile = "medium"
				raw, err := kubernetes.MarshalIntent(cfg)
				require.NoError(t, err)
				assert.Equal(t, raw, after.Annotations[kubernetes.AnnotationDesiredState], "profile selection must not fill unspecified intent fields")
			} else {
				require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), "recorded_intent_required")
				assert.Equal(t, before, after, "rejected profile selection must not adopt or write")
			}
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
			assert.Equal(t, secret.Data, storedSecret.Data)
		})
	}
}
