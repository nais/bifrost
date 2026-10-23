package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/nais/bifrost/pkg/domain/unleash"
	fqdn "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func tunedInstance(t *testing.T) (*unleashv1.Unleash, *unleash.Config) {
	t.Helper()
	cfg := testIntentConfig()
	live := BuildUnleashCRD(repoTestConfig(), cfg)
	live.Spec.Size = 4
	live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("768Mi")
	live.Spec.Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("2Gi")
	live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
	live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
	live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
	live.Spec.ExtraContainers[0].Args = append(live.Spec.ExtraContainers[0].Args, "--private-ip")
	return &live, cfg
}

func TestResourceProfilesOnlyOwnThreeResourceKeys(t *testing.T) {
	for _, profile := range []string{"small", "medium", "large"} {
		t.Run(profile, func(t *testing.T) {
			live, cfg := tunedInstance(t)
			cfg.ResourceProfile = profile
			desired := BuildUnleashCRD(repoTestConfig(), cfg)
			MergeResourceOwnership(live, &desired, profile, true)
			assert.True(t, unleash.ResourcesMatchProfile(desired.Spec.Resources, profile))
			assert.Equal(t, live.Spec.Resources.Limits[corev1.ResourceCPU], desired.Spec.Resources.Limits[corev1.ResourceCPU])
			assert.Equal(t, live.Spec.Resources.Requests[corev1.ResourceEphemeralStorage], desired.Spec.Resources.Requests[corev1.ResourceEphemeralStorage])
			assert.Equal(t, live.Spec.Size, desired.Spec.Size)
			assert.Equal(t, live.Spec.ExtraContainers, desired.Spec.ExtraContainers)
			assert.Equal(t, resource.MustParse("768Mi"), live.Spec.Resources.Requests[corev1.ResourceMemory], "merge must not alias its input")
		})
	}
}

func TestProfileRepositoryWritesPreserveLiveTuningAndIntent(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmtBool(explicit)+"/enabled="+fmtBool(enabled), func(t *testing.T) {
				ctx := context.Background()
				live, cfg := tunedInstance(t)
				if explicit {
					cfg.ResourceProfile = "large"
					raw, err := MarshalIntent(cfg)
					require.NoError(t, err)
					live.Annotations[AnnotationDesiredState] = raw
				}
				client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).WithObjects(live, &fqdn.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: live.Name + "-fqdn", Namespace: live.Namespace}}).Build()
				settings := repoTestConfig()
				settings.Unleash.ResourceProfilesEnabled = enabled
				logger := logrus.New()
				logger.SetOutput(io.Discard)
				repo := NewUnleashRepository(client, settings, logger)
				next := *cfg
				next.ResourceProfile = "" // unrelated writer knows nothing about profiles
				next.LogLevel = "debug"
				require.NoError(t, repo.Update(ctx, &next, unleash.UpdateOptions{}))
				after, err := repo.GetCRD(ctx, live.Name)
				require.NoError(t, err)
				want := BuildUnleashCRD(settings, &next)
				MergeResourceOwnership(live, &want, cfg.ResourceProfile, enabled)
				assert.Equal(t, want.Spec, after.Spec)
				intent, err := UnmarshalIntent(after.Annotations[AnnotationDesiredState])
				require.NoError(t, err)
				assert.Equal(t, cfg.ResourceProfile, intent.ResourceProfile)
			})
		}
	}
}

func fmtBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestProfileOnlyUpdatePreservesWholeSpecAndConflicts(t *testing.T) {
	ctx := context.Background()
	live, cfg := tunedInstance(t)
	live.Spec.WebIngress.Host = "manually-tuned.example"
	client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).WithObjects(live).Build()
	settings := repoTestConfig()
	settings.Unleash.ResourceProfilesEnabled = true
	repo := NewUnleashRepository(client, settings, logrus.New())
	read, err := repo.GetCRD(ctx, live.Name)
	require.NoError(t, err)
	cfg.ResourceProfile = "medium"
	require.NoError(t, repo.Update(ctx, cfg, unleash.UpdateOptions{ExpectedResourceVersion: read.ResourceVersion}))
	after, err := repo.GetCRD(ctx, live.Name)
	require.NoError(t, err)
	want := live.DeepCopy()
	MergeResourceOwnership(live, want, "medium", true)
	assert.Equal(t, want.Spec, after.Spec, "only three resource keys may change")
	cfg.ResourceProfile = "large"
	err = repo.Update(ctx, cfg, unleash.UpdateOptions{ExpectedResourceVersion: read.ResourceVersion})
	require.True(t, apierrors.IsConflict(err), "%v", err)
	unchanged, err := repo.GetCRD(ctx, live.Name)
	require.NoError(t, err)
	assert.Equal(t, after, unchanged)
}

func TestIntentProfileCompatibilityAndBytes(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		cfg := testIntentConfig()
		bytes, err := json.Marshal(cfg)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(bytes, &fields))
		if version != 0 {
			fields["schemaVersion"] = version
		}
		bytes, err = json.MarshalIndent(fields, "", "  ")
		require.NoError(t, err)
		raw := string(bytes)
		parsed, err := UnmarshalIntent(raw)
		require.NoError(t, err)
		assert.Empty(t, parsed.ResourceProfile)
		rendered := BuildUnleashCRD(repoTestConfig(), parsed)
		live := rendered.DeepCopy()
		live.Annotations[AnnotationDesiredState] = raw
		ApplyManagedMetadata(live, &rendered)
		assert.Equal(t, raw, live.Annotations[AnnotationDesiredState])
	}
	for _, raw := range []string{`null`, `{`, `{"schemaVersion":null}`, `{"schemaVersion":99}`, `{"schemaVersion":2,"ResourceProfile":null}`, `{"schemaVersion":2,"ResourceProfile":""}`, `{"schemaVersion":2,"ResourceProfile":"huge"}`, `{"schemaVersion":1,"ResourceProfile":"small"}`, `{"schemaVersion":2,"resourceprofile":"small"}`, `{"SchemaVersion":2}`, `{"schemaVersion":2,"unknown":true}`} {
		_, err := UnmarshalIntent(raw)
		require.Error(t, err, raw)
	}
}

func TestDisabledAndInvalidProfileSelectionMakesNoWrite(t *testing.T) {
	for _, profile := range []string{"medium", "invalid"} {
		live, cfg := tunedInstance(t)
		client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).WithObjects(live).Build()
		repo := NewUnleashRepository(client, repoTestConfig(), logrus.New())
		before, err := repo.GetCRD(context.Background(), live.Name)
		require.NoError(t, err)
		cfg.ResourceProfile = profile
		require.Error(t, repo.Update(context.Background(), cfg, unleash.UpdateOptions{}))
		after, err := repo.GetCRD(context.Background(), live.Name)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		var list unleashv1.UnleashList
		require.NoError(t, client.List(context.Background(), &list, ctrl.InNamespace(live.Namespace)))
		assert.Len(t, list.Items, 1)
	}
}

func TestCreatePersistsExplicitProfileIncludingDefault(t *testing.T) {
	for _, profile := range []string{"", "small", "medium", "large"} {
		client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).Build()
		settings := repoTestConfig()
		settings.Unleash.ResourceProfilesEnabled = profile != ""
		repo := NewUnleashRepository(client, settings, logrus.New())
		cfg := testIntentConfig()
		cfg.ResourceProfile = profile
		require.NoError(t, repo.Create(context.Background(), cfg))
		crd, err := repo.GetCRD(context.Background(), cfg.Name)
		require.NoError(t, err)
		intent, err := UnmarshalIntent(crd.Annotations[AnnotationDesiredState])
		require.NoError(t, err)
		assert.Equal(t, unleash.EffectiveResourceProfile(profile), intent.ResourceProfile)
		assert.Equal(t, unleash.ProfileResources(profile), crd.Spec.Resources)
		assert.Equal(t, profile, cfg.ResourceProfile, "create must not mutate caller intent")
	}
}

func TestVersionChannelChangesRetainResourceOwnershipWhenDisabled(t *testing.T) {
	for _, profile := range []string{"", "large"} {
		live, cfg := tunedInstance(t)
		cfg.ResourceProfile = profile
		raw, err := MarshalIntent(cfg)
		require.NoError(t, err)
		live.Annotations[AnnotationDesiredState] = raw
		client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).WithObjects(live, &fqdn.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: live.Name + "-fqdn", Namespace: live.Namespace}}).Build()
		repo := NewUnleashRepository(client, repoTestConfig(), logrus.New())
		for _, channel := range []string{"stable-v6", "stable-v7"} {
			cfg.CustomVersion = ""
			cfg.ReleaseChannelName = channel
			require.NoError(t, repo.Update(context.Background(), cfg, unleash.UpdateOptions{}))
			after, err := repo.GetCRD(context.Background(), live.Name)
			require.NoError(t, err)
			assert.Equal(t, live.Spec.Resources, after.Spec.Resources)
			assert.Equal(t, live.Spec.ExtraContainers, after.Spec.ExtraContainers)
			assert.Equal(t, live.Spec.Size, after.Spec.Size)
			assert.Equal(t, live.Spec.Database, after.Spec.Database, "database and credential references must not change")
			intent, err := UnmarshalIntent(after.Annotations[AnnotationDesiredState])
			require.NoError(t, err)
			assert.Equal(t, profile, intent.ResourceProfile)
			assert.Equal(t, channel, intent.ReleaseChannelName)
		}
		require.NoError(t, repo.Delete(context.Background(), cfg.Name))
		_, err = repo.GetCRD(context.Background(), cfg.Name)
		assert.True(t, apierrors.IsNotFound(err), "disabled feature must not block lifecycle deletion")
	}
}

func TestProfileUpdateRefusesMalformedRecordedIntentWithoutWrites(t *testing.T) {
	for _, raw := range []string{`null`, `{`, `{"schemaVersion":99}`, `{"schemaVersion":2,"ResourceProfile":null}`} {
		live, cfg := tunedInstance(t)
		live.Annotations[AnnotationDesiredState] = raw
		client := fake.NewClientBuilder().WithScheme(repoTestScheme(t)).WithObjects(live).Build()
		settings := repoTestConfig()
		settings.Unleash.ResourceProfilesEnabled = true
		repo := NewUnleashRepository(client, settings, logrus.New())
		before, err := repo.GetCRD(context.Background(), live.Name)
		require.NoError(t, err)
		cfg.ResourceProfile = "medium"
		require.Error(t, repo.Update(context.Background(), cfg, unleash.UpdateOptions{}))
		after, err := repo.GetCRD(context.Background(), live.Name)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
}
