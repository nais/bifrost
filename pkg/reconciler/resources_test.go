package reconciler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestProfileReconcileOwnershipAndDisable(t *testing.T) {
	for _, profile := range []string{"", "small", "medium", "large"} {
		for _, enabled := range []bool{false, true} {
			t.Run(profile+"/enabled="+map[bool]string{true: "true", false: "false"}[enabled], func(t *testing.T) {
				cfg := intentFor(t, "team-a")
				cfg.ResourceProfile = profile
				live := kubernetes.BuildUnleashCRD(testConfig(), cfg)
				live.Spec.Size = 3
				live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
				live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
				live.Spec.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
				live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
				live.Spec.ExtraContainers[0].Args = []string{"tuned-proxy"}
				live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
				live.Spec.ExtraContainers[0].SecurityContext = nil
				live.Spec.ApiIngress.Class = "drifted"
				c := newFakeClient(t, &live)
				r := newReconciler(c)
				r.config.Unleash.ResourceProfilesEnabled = enabled
				r.config.CloudConnectorProxy = "example.com/cloud-sql-proxy:new"
				want := live.DeepCopy()
				want.Spec.ApiIngress.Class = testConfig().Unleash.InstanceAPIIngressClass
				want.Spec.ExtraContainers = kubernetes.BuildUnleashCRD(r.config, cfg).Spec.ExtraContainers
				require.NoError(t, kubernetes.MergeResourceOwnership(&live, want, profile, enabled))
				// Merge patches omit empty slices, just as the API server does.
				raw, err := json.Marshal(want)
				require.NoError(t, err)
				var storedWant unleashv1.Unleash
				require.NoError(t, json.Unmarshal(raw, &storedWant))
				require.NoError(t, reconcileOnce(r, &live))
				after := &unleashv1.Unleash{}
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&live), after))
				assert.Equal(t, storedWant.Spec, after.Spec, "unrelated drift still converges while tuning survives")
				assert.Equal(t, r.config.CloudConnectorProxy, after.Spec.ExtraContainers[0].Image, "configured proxy upgrade must reach the controller spec")
				assert.Equal(t, live.Spec.ExtraContainers[0].Resources, after.Spec.ExtraContainers[0].Resources)
				assert.NotEqual(t, live.Spec.ExtraContainers[0].Args, after.Spec.ExtraContainers[0].Args)
				assert.NotNil(t, after.Spec.ExtraContainers[0].SecurityContext)
				require.NoError(t, reconcileOnce(r, after))
				again := &unleashv1.Unleash{}
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(after), again))
				assert.Equal(t, after, again, "second reconcile must not write")
			})
		}
	}
}

func reconcileOnce(r *UnleashReconciler, crd *unleashv1.Unleash) error {
	_, err := r.Reconcile(context.Background(), requestFor(crd))
	return err
}

func TestProfileIntroductionDoesNotWriteLegacyAnnotations(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		live := renderManaged(t, "team-a")
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(live.Annotations[kubernetes.AnnotationDesiredState]), &fields))
		fields["FederationNonce"] = ""
		if version == 0 {
			delete(fields, "schemaVersion")
		} else {
			fields["schemaVersion"] = version
		}
		raw, err := json.MarshalIndent(fields, "", "  ")
		require.NoError(t, err)
		live.Annotations[kubernetes.AnnotationDesiredState] = string(raw)
		live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
		live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
		c := newFakeClient(t, &live)
		r := newReconciler(c)
		r.config.Unleash.ResourceProfilesEnabled = true
		before := &unleashv1.Unleash{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&live), before))
		require.NoError(t, reconcileOnce(r, before))
		after := &unleashv1.Unleash{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&live), after))
		assert.Equal(t, before, after, "inheritance must not write spec or annotation bytes")
	}
}

func TestIncompatibleProfileLimitReconcileFailsClosed(t *testing.T) {
	for _, profile := range []string{"", "medium"} {
		for _, enabled := range []bool{false, true} {
			t.Run(profile+"/enabled="+map[bool]string{true: "true", false: "false"}[enabled], func(t *testing.T) {
				cfg := intentFor(t, "team-a")
				cfg.ResourceProfile = profile
				live := kubernetes.BuildUnleashCRD(testConfig(), cfg)
				live.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("25m")
				live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("50m")
				if profile != "" && enabled {
					live.Spec.ApiIngress.Class = "unrelated-drift"
				}
				c := newFakeClient(t, &live)
				r := newReconciler(c)
				r.config.Unleash.ResourceProfilesEnabled = enabled
				before := get(t, c, live.Namespace, live.Name)
				require.NoError(t, reconcileOnce(r, before))
				after := get(t, c, live.Namespace, live.Name)
				assert.Equal(t, before, after, "incompatible enforcement must not apply even unrelated changes; absent/disabled ownership preserves resources")
			})
		}
	}
}

func TestProfileReconcileFailsClosedWithoutUsableIntent(t *testing.T) {
	for _, raw := range []string{"", `null`, `{`, `{"schemaVersion":99}`, `{"schemaVersion":2,"ResourceProfile":null}`} {
		live := renderManaged(t, "team-a")
		live.Annotations[kubernetes.AnnotationDesiredState] = raw
		live.Spec.Size = 5
		live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
		c := newFakeClient(t, &live)
		r := newReconciler(c)
		r.config.Unleash.ResourceProfilesEnabled = true
		before := &unleashv1.Unleash{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&live), before))
		require.NoError(t, reconcileOnce(r, before))
		after := &unleashv1.Unleash{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(&live), after))
		assert.Equal(t, before, after, "missing/malformed intent must not write or adopt")
	}
}

func TestLegacyAdoptionKeepsResourceOwnershipLive(t *testing.T) {
	live := legacyInstance(t, "team-a")
	live.Spec.Size = 4
	live.Spec.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("777Mi")}}
	live.Spec.ExtraContainers[0].Args = []string{"tuned-proxy"}
	live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
	c := newFakeClient(t, live)
	r := adoptionReconciler(c, true, false)
	r.config.Unleash.ResourceProfilesEnabled = true
	r.adoptFleet(context.Background())
	after := get(t, c, live.Namespace, live.Name)
	assert.Equal(t, live.Spec.Resources, after.Spec.Resources)
	assert.Equal(t, live.Spec.Size, after.Spec.Size)
	assert.Equal(t, live.Spec.ExtraContainers[0].Resources, after.Spec.ExtraContainers[0].Resources)
	assert.Equal(t, kubernetes.BuildUnleashCRD(r.config, intentFor(t, "team-a")).Spec.ExtraContainers[0].Args, after.Spec.ExtraContainers[0].Args)
	intent, err := kubernetes.UnmarshalIntent(after.Annotations[kubernetes.AnnotationDesiredState])
	require.NoError(t, err)
	assert.Empty(t, intent.ResourceProfile, "existing opt-in adoption must not select a profile")
}
