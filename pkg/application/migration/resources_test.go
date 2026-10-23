package migration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/nais/bifrost/pkg/reconciler"
	fqdn "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProfileSchemaResumesLegacyInFlightTransaction(t *testing.T) {
	// Frozen schema-1 serialization: old binaries hashed this, not the annotation bytes.
	const legacy = `{"schemaVersion":1,"Name":"team-a","CustomVersion":"","ReleaseChannelName":"stable-v6","EnableFederation":false,"FederationNonce":"","AllowedTeams":"","AllowedNamespaces":"","AllowedClusters":"","LogLevel":"warn","DatabasePoolMax":3,"DatabasePoolIdleTimeoutMs":1000}`
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			repo := NewMockUnleashRepository()
			repo.AddInstance("team-a", "", "stable-v6", true)
			channels := channelTestChannels()
			installTransaction(t, repo, channels, "team-a", phasePrepared, time.Now().Add(time.Minute))
			tx := mustTransaction(t, repo, "team-a")
			targetRaw := strings.Replace(legacy, "stable-v6", "stable-v7", 1)
			tx.DesiredStateIntentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
			tx.TargetDesiredStateIntentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(targetRaw)))
			raw, err := marshalChannelMigrationTransaction(tx)
			require.NoError(t, err)
			setDesiredChannel(t, repo, "team-a", "stable-v7")
			if version == 0 {
				targetRaw = strings.Replace(targetRaw, `"schemaVersion":1,`, "", 1)
			}
			repo.mu.Lock()
			repo.crds["team-a"].Annotations[kubernetes.AnnotationDesiredState] = targetRaw
			repo.crds["team-a"].Annotations[channelMigrationAnnotation] = raw
			repo.mu.Unlock()
			repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")
			cfg := newChannelTestConfig(false, "", time.Second)
			newChannelTestReconciler(repo, channels, cfg).Start(context.Background())
			completed := mustTransaction(t, repo, "team-a")
			assert.Equal(t, phaseCompleted, completed.Phase)
			assert.Equal(t, tx.Deadline, completed.Deadline, "resume the pinned transaction, do not restart it")
			assert.Equal(t, tx.TargetDesiredStateIntentHash, completed.TargetDesiredStateIntentHash)
			assert.Zero(t, repo.updateAttempts, "already-written target must be recognized under both old schemas")
		})
	}
}

func TestPreparedChannelMigrationWritesAndRollsBackWithEmptyNonceIntent(t *testing.T) {
	for _, profile := range []string{"", "large"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("profile=%s/enabled=%t", profile, enabled), func(t *testing.T) {
				ctx := context.Background()
				cfg := newChannelTestConfig(false, "", time.Minute)
				cfg.Unleash.ResourceProfilesEnabled = enabled
				cfg.Unleash.SQLInstanceAddress = "10.0.0.1"
				cfg.Unleash.SQLInstanceID = "shared-sql"
				cfg.Unleash.SQLInstanceRegion = "europe-north1"
				cfg.Unleash.InstanceWebIngressHost = "web.example"
				cfg.Unleash.InstanceAPIIngressHost = "api.example"
				intent := validIntent("team-a", "stable-v6")
				intent.EnableFederation = true
				intent.ResourceProfile = profile
				render := *intent
				render.FederationNonce = "livetestnonce"
				live := kubernetes.BuildUnleashCRD(cfg, &render)
				live.UID = types.UID("instance-uid")
				live.Generation = 1
				live.Spec.Size = 4
				live.Spec.Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
				live.Spec.ExtraContainers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("333Mi")
				if profile == "" {
					live.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("777Mi")
					live.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
				}
				for _, condition := range []string{unleashv1.UnleashStatusConditionTypeReconciled, unleashv1.UnleashStatusConditionTypeConnected} {
					live.Status.Conditions = append(live.Status.Conditions, metav1.Condition{Type: condition, Status: metav1.ConditionTrue, ObservedGeneration: 1, LastTransitionTime: metav1.Now()})
				}
				raw, err := kubernetes.MarshalIntent(intent)
				require.NoError(t, err)
				live.Annotations[kubernetes.AnnotationDesiredState] = raw
				_, sourceHash, err := loadDesiredStateIntent(&live)
				require.NoError(t, err)
				_, targetHash, err := targetIntent(intent, "stable-v7")
				require.NoError(t, err)
				channels := channelTestChannels()
				source, target := channels.channels["stable-v6"], channels.channels["stable-v7"]
				tx := &channelMigrationTransaction{
					SchemaVersion: channelTransactionSchema, ResourceUID: live.UID, Phase: phasePrepared, Deadline: time.Now().UTC().Add(time.Minute),
					SourceChannel: source.Name, SourceChannelUID: source.UID, SourceImage: source.Image,
					TargetChannel: target.Name, TargetChannelUID: target.UID, TargetImage: target.Image,
					DesiredStateIntentHash: sourceHash, TargetDesiredStateIntentHash: targetHash,
				}
				transactionRaw, err := marshalChannelMigrationTransaction(tx)
				require.NoError(t, err)
				live.Annotations[channelMigrationAnnotation] = transactionRaw
				scheme := runtime.NewScheme()
				require.NoError(t, unleashv1.AddToScheme(scheme))
				require.NoError(t, fqdn.AddToScheme(scheme))
				require.NoError(t, corev1.AddToScheme(scheme))
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: live.Name, Namespace: live.Namespace}, Data: map[string][]byte{"password": []byte("unchanged-test-value")}}
				kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&live, secret, &fqdn.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: live.Name + "-fqdn", Namespace: live.Namespace}}).Build()
				beforeSecret := &corev1.Secret{}
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(secret), beforeSecret))
				repo := kubernetes.NewUnleashRepository(kube, cfg, newTestLogger())
				engine := newChannelTestReconciler(repo, channels, cfg)
				before, err := repo.GetCRD(ctx, live.Name)
				require.NoError(t, err)
				for _, rollback := range []bool{false, true} {
					wantChannel, wantHash, wantPhase := "stable-v7", targetHash, phaseTargetWritten
					if rollback {
						require.NoError(t, engine.transition(ctx, live.Name, []channelMigrationPhase{phaseTargetWritten}, phaseRollbackPending, []string{targetHash}, time.Time{}, ""))
						require.NoError(t, engine.ensureRollbackWritten(ctx, live.Name))
						wantChannel, wantHash, wantPhase = "stable-v6", sourceHash, phaseRollbackWritten
					} else {
						require.NoError(t, engine.ensureTargetWritten(ctx, live.Name))
					}
					state, err := engine.readTransaction(ctx, live.Name)
					require.NoError(t, err)
					assert.Equal(t, wantPhase, state.transaction.Phase)
					assert.Equal(t, wantHash, state.intentHash, "forward and rollback hashes must retain the original unspecified nonce")
					assert.Empty(t, state.intent.FederationNonce)
					want := before.DeepCopy()
					want.Spec.ReleaseChannel.Name = wantChannel
					assert.Equal(t, want.Spec, state.crd.Spec, "only the version source changes; replicas, sidecars, ingress, database and live nonce stay intact")
					assert.NotEqual(t, before.ResourceVersion, state.crd.ResourceVersion)
					r := reconciler.NewUnleashReconciler(kube, cfg, newTestLogger(), time.Minute, false)
					for range 2 {
						_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&live)})
						require.NoError(t, err)
						current, err := repo.GetCRD(ctx, live.Name)
						require.NoError(t, err)
						assert.Equal(t, state.crd, current, "reconcile must not rewrite transaction intent, hash or credentials")
					}
				}
				storedSecret := &corev1.Secret{}
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(secret), storedSecret))
				assert.Equal(t, beforeSecret, storedSecret)
				assert.Empty(t, intent.FederationNonce, "rendering must not mutate the caller's intent")
			})
		}
	}
}
