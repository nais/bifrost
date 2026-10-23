package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	appunleash "github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	fqdn "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
