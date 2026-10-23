package dto

import (
	"encoding/json"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResourceProfileRequestPresence(t *testing.T) {
	for _, tc := range []struct {
		body        string
		profile     string
		valid, only bool
	}{
		{`{}`, "", true, false},
		{`{"resource_profile":"small"}`, "small", true, true},
		{`{"resource_profile":"medium","name":"team-a"}`, "medium", true, true},
		{`{"resource_profile":"large","log_level":"debug"}`, "large", true, false},
		{`{"resource_profile":""}`, "", false, false},
		{`{"resource_profile":null}`, "", false, false},
		{`{"resource_profile":"Small"}`, "", false, false},
		{`{"resource_profile":"huge"}`, "", false, false},
		{`{"RESOURCE_PROFILE":""}`, "", false, false},
		{`{"RESOURCE_PROFILE":null}`, "", false, false},
		{`{"RESOURCE_PROFILE":"huge"}`, "", false, false},
		{`{"Resource_Profile":""}`, "", false, false},
		{`{"Resource_Profile":null}`, "", false, false},
		{`{"Resource_Profile":"huge"}`, "", false, false},
		{`{"RESOURCE_PROFILE":"small"}`, "", false, false},
		{`{"resource_profile":"small","Resource_Profile":"large"}`, "", false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var req UnleashConfigRequest
			err := json.Unmarshal([]byte(tc.body), &req)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.profile, derefString(req.ResourceProfile))
			assert.Equal(t, tc.only, req.ProfileOnly)
		})
	}
}

func TestProfileResponseReportsActualResources(t *testing.T) {
	cfg, err := unleash.NewConfigBuilder().WithName("team-a").Build()
	require.NoError(t, err)
	for _, profile := range []string{"", "small", "large"} {
		cfg.ResourceProfile = profile
		crd := kubernetes.BuildUnleashCRD(&config.Config{}, cfg)
		response := ToCRDResponse(&crd)
		assert.Equal(t, unleash.EffectiveResourceProfile(profile), response.ResourceProfile)
		assert.Equal(t, profile != "", response.ResourceProfileExplicit)
		assert.True(t, response.ResourceProfileMatches)
		crd.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
		response = ToCRDResponse(&crd)
		assert.False(t, response.ResourceProfileMatches)
		assert.Equal(t, crd.Spec.Resources, response.ResolvedResources)
		bytes, err := json.Marshal(response)
		require.NoError(t, err)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(bytes, &fields))
		assert.Contains(t, fields, "spec", "existing CRD shape stays at the top level")
		assert.Contains(t, fields, "resolved_resources")
	}
}
