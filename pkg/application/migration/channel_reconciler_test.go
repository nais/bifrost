package migration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newChannelTestConfig(enabled bool, channelMap string, healthTimeout time.Duration) *config.Config {
	return &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:             "unleash",
			ChannelMigrationEnabled:       enabled,
			ChannelMigrationMap:           channelMap,
			ChannelMigrationHealthTimeout: healthTimeout,
			ChannelMigrationMaxCandidates: 10,
			ChannelMigrationRollbackSafe:  false,
		},
	}
}

func newChannelTestReconciler(
	repo unleash.Repository,
	channelRepo *MockReleaseChannelRepository,
	cfg *config.Config,
) *ChannelReconciler {
	reconciler := NewChannelReconciler(repo, channelRepo, cfg, newTestLogger())
	reconciler.pollInterval = 5 * time.Millisecond
	reconciler.retryDelay = time.Millisecond
	return reconciler
}

func TestChannelMigrationTransactionRequiresKnownVersionAndUID(t *testing.T) {
	transaction := &channelMigrationTransaction{
		SchemaVersion:                channelTransactionSchema,
		ResourceUID:                  "instance-uid",
		Phase:                        phasePrepared,
		Deadline:                     time.Now().UTC().Add(time.Minute),
		SourceChannel:                "stable-v6",
		TargetChannel:                "stable-v7",
		TargetChannelUID:             "channel-uid",
		TargetImage:                  "unleash/unleash-server:7.6.5",
		DesiredStateIntentHash:       "source-hash",
		TargetDesiredStateIntentHash: "target-hash",
	}

	raw, err := marshalChannelMigrationTransaction(transaction)
	require.NoError(t, err)
	decoded, err := unmarshalChannelMigrationTransaction(raw)
	require.NoError(t, err)
	assert.Equal(t, transaction, decoded)

	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &document))
	assert.EqualValues(t, channelTransactionSchema, document["schemaVersion"])

	document["schemaVersion"] = channelTransactionSchema + 1
	unknownVersion, err := json.Marshal(document)
	require.NoError(t, err)
	_, err = unmarshalChannelMigrationTransaction(string(unknownVersion))
	require.Error(t, err)

	transaction.SchemaVersion = channelTransactionSchema
	transaction.ResourceUID = ""
	_, err = marshalChannelMigrationTransaction(transaction)
	require.Error(t, err)
}

func TestChannelReconcilerAdmitsOnlyManagedValidIntentWithinCanaryCap(t *testing.T) {
	repo := NewMockUnleashRepository()
	for _, name := range []string{"alpha", "bravo", "charlie", "invalid", "unmanaged"} {
		repo.AddInstance(name, "", "stable-v6", true)
		repo.SetReadyOnChannel(name, "stable-v7", "unleash/unleash-server:7.6.5")
	}
	repo.mu.Lock()
	repo.instances["alpha"].IsReady = false
	repo.mu.Unlock()

	repo.mu.Lock()
	delete(repo.crds["invalid"].Annotations, kubernetes.AnnotationDesiredState)
	delete(repo.crds["unmanaged"].Labels, kubernetes.LabelManagedBy)
	repo.mu.Unlock()

	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)
	cfg.Unleash.ChannelMigrationMaxCandidates = 2

	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	assert.Equal(t, "stable-v6", mustInstance(t, repo, "alpha").ReleaseChannelName)
	assert.Equal(t, "stable-v7", mustInstance(t, repo, "bravo").ReleaseChannelName)
	assert.Equal(t, "stable-v7", mustInstance(t, repo, "charlie").ReleaseChannelName)
	assert.Equal(t, "stable-v6", mustInstance(t, repo, "invalid").ReleaseChannelName)
	assert.Equal(t, "stable-v6", mustInstance(t, repo, "unmanaged").ReleaseChannelName)
	assert.Equal(t, []string{"bravo", "charlie"}, repo.updateCalls)
}

func TestChannelReconcilerPersistsPinnedCompletedTransactionAndPreservesObjectState(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")

	repo.mu.Lock()
	repo.crds["team-a"].Finalizers = []string{"unleash.nais.io/finalizer"}
	repo.crds["team-a"].Labels["foreign-label"] = "keep"
	repo.crds["team-a"].Annotations["foreign-annotation"] = "keep"
	repo.crds["team-a"].Status.Version = "6.4.0"
	repo.mu.Unlock()

	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)
	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseCompleted, transaction.Phase)
	assert.Equal(t, types.UID("uid-team-a"), transaction.ResourceUID)
	assert.Equal(t, "stable-v6", transaction.SourceChannel)
	assert.Equal(t, "stable-v7", transaction.TargetChannel)
	assert.Equal(t, types.UID("uid-stable-v7"), transaction.TargetChannelUID)
	assert.Equal(t, "unleash/unleash-server:7.6.5", transaction.TargetImage)
	assert.NotEmpty(t, transaction.DesiredStateIntentHash)
	assert.NotEmpty(t, transaction.TargetDesiredStateIntentHash)
	assert.False(t, transaction.Deadline.IsZero())
	require.Len(t, repo.updateOptions, 1)
	assert.NotEmpty(t, repo.updateOptions[0].ExpectedResourceVersion)
	assert.Equal(t, []channelMigrationPhase{phasePrepared, phaseTargetWritten, phaseCompleted}, repo.transactionPhases)

	crd, err := repo.GetCRD(context.Background(), "team-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"unleash.nais.io/finalizer"}, crd.Finalizers)
	assert.Equal(t, "keep", crd.Labels["foreign-label"])
	assert.Equal(t, "keep", crd.Annotations["foreign-annotation"])
	assert.Equal(t, "6.4.0", crd.Status.Version)
}

func TestChannelReconcilerResumesPreparedTransactionAfterTargetWrite(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	installTransaction(t, repo, channels, "team-a", phasePrepared, time.Now().Add(time.Minute))
	setDesiredChannel(t, repo, "team-a", "stable-v7")
	repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")

	cfg := newChannelTestConfig(false, "", time.Second)
	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	assert.Equal(t, phaseCompleted, mustTransaction(t, repo, "team-a").Phase)
	assert.Zero(t, repo.updateAttempts, "recovery must recognize the already-written target intent")
}

func TestChannelReconcilerRequiresManualRecoveryOnTargetTimeoutByDefault(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", 25*time.Millisecond)

	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseManualRecovery, transaction.Phase)
	assert.Equal(t, failureTargetTimeout, transaction.FailureReason)
	assert.Equal(t, "stable-v7", mustInstance(t, repo, "team-a").ReleaseChannelName)
	assert.Equal(t, 1, repo.updateAttempts)
}

func TestChannelReconcilerPersistsManualRecoveryWhenTargetWriteFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	repo.updateErr = errors.New("target write failed")
	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)

	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseManualRecovery, transaction.Phase)
	assert.Equal(t, failureTargetWrite, transaction.FailureReason)
	assert.Equal(t, "stable-v6", mustInstance(t, repo, "team-a").ReleaseChannelName)
	assert.Equal(t, 1, repo.updateAttempts)
}

func TestChannelReconcilerRollsBackOnlyWithExplicitSafeConfiguration(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	repo.SetReadyOnChannel("team-a", "stable-v6", "unleash/unleash-server:6.4.0")
	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", 25*time.Millisecond)
	cfg.Unleash.ChannelMigrationRollbackSafe = true

	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseRolledBack, transaction.Phase)
	assert.Equal(t, "stable-v6", mustInstance(t, repo, "team-a").ReleaseChannelName)
	assert.Equal(t, 2, repo.updateAttempts)
}

func TestChannelReconcilerNeverRollsBackWithCanceledContext(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Minute)
	cfg.Unleash.ChannelMigrationRollbackSafe = true
	reconciler := newChannelTestReconciler(repo, channels, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		reconciler.Start(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		transaction, err := transactionFromRepo(repo, "team-a")
		return err == nil && transaction.Phase == phaseTargetWritten
	}, time.Second, 5*time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel reconciler did not stop after cancellation")
	}

	assert.Equal(t, phaseTargetWritten, mustTransaction(t, repo, "team-a").Phase)
	assert.Equal(t, 1, repo.updateAttempts)
	assert.Equal(t, "stable-v7", mustInstance(t, repo, "team-a").ReleaseChannelName)
}

func TestChannelReconcilerRetriesConflictsWithFreshSemanticValidation(t *testing.T) {
	t.Run("annotation conflicts", func(t *testing.T) {
		repo := NewMockUnleashRepository()
		repo.AddInstance("team-a", "", "stable-v6", true)
		repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")
		repo.patchConflicts = 2
		channels := channelTestChannels()
		cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)

		newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

		assert.Equal(t, phaseCompleted, mustTransaction(t, repo, "team-a").Phase)
		assert.Equal(t, 5, repo.patchAttempts)
	})

	t.Run("transient conflicts", func(t *testing.T) {
		repo := NewMockUnleashRepository()
		repo.AddInstance("team-a", "", "stable-v6", true)
		repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")
		repo.updateConflicts = 2
		channels := channelTestChannels()
		cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)

		newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

		assert.Equal(t, phaseCompleted, mustTransaction(t, repo, "team-a").Phase)
		assert.Equal(t, 3, repo.updateAttempts)
	})

	t.Run("intent changed during conflict", func(t *testing.T) {
		repo := NewMockUnleashRepository()
		repo.AddInstance("team-a", "", "stable-v6", true)
		repo.SetConflictIntentOnNextUpdate("team-a", validIntent("team-a", "rapid-v7"))
		channels := channelTestChannels()
		cfg := newChannelTestConfig(true, "stable-v6:stable-v7", time.Second)

		newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

		assert.Equal(t, phasePrepared, mustTransaction(t, repo, "team-a").Phase)
		assert.Equal(t, "rapid-v7", mustInstance(t, repo, "team-a").ReleaseChannelName)
		assert.Equal(t, 1, repo.updateAttempts)
		assert.Empty(t, repo.updateCalls, "the changed user intent must not be overwritten")
	})
}

func TestChannelReconcilerDetectsPinnedTargetChangeOnRecovery(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	installTransaction(t, repo, channels, "team-a", phaseTargetWritten, time.Now().Add(time.Minute))
	setDesiredChannel(t, repo, "team-a", "stable-v7")
	channels.channels["stable-v7"].Image = "unleash/unleash-server:7.7.0"

	cfg := newChannelTestConfig(false, "", time.Second)
	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseManualRecovery, transaction.Phase)
	assert.Equal(t, failureTargetChannelChanged, transaction.FailureReason)
	assert.Zero(t, repo.updateAttempts)
}

func TestChannelReconcilerDoesNotAcceptStaleReadyStatusFromSourceImage(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	installTransaction(t, repo, channels, "team-a", phaseTargetWritten, time.Now().Add(25*time.Millisecond))
	setDesiredChannel(t, repo, "team-a", "stable-v7")

	repo.mu.Lock()
	repo.instances["team-a"].IsReady = true
	repo.instances["team-a"].ChannelNameFromStatus = "stable-v6"
	repo.instances["team-a"].ResolvedImage = "unleash/unleash-server:6.4.0"
	repo.mu.Unlock()

	cfg := newChannelTestConfig(false, "", 25*time.Millisecond)
	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	transaction := mustTransaction(t, repo, "team-a")
	assert.Equal(t, phaseManualRecovery, transaction.Phase)
	assert.Equal(t, failureTargetTimeout, transaction.FailureReason)
}

func TestChannelReconcilerRefusesTransactionForRecreatedResourceUID(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	channels := channelTestChannels()
	installTransaction(t, repo, channels, "team-a", phasePrepared, time.Now().Add(time.Minute))

	repo.mu.Lock()
	repo.crds["team-a"].UID = "replacement-uid"
	initialPatchCalls := len(repo.patchCalls)
	repo.mu.Unlock()

	cfg := newChannelTestConfig(false, "", time.Second)
	newChannelTestReconciler(repo, channels, cfg).Start(context.Background())

	assert.Equal(t, phasePrepared, mustTransaction(t, repo, "team-a").Phase)
	assert.Len(t, repo.patchCalls, initialPatchCalls)
	assert.Zero(t, repo.updateAttempts)
}

func TestParseChannelMigrationMap(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty", input: "", want: map[string]string{}},
		{name: "single mapping", input: "stable-v6:stable-v7", want: map[string]string{"stable-v6": "stable-v7"}},
		{
			name:  "multiple mappings",
			input: "stable-v6:stable-v7,rapid-v6:rapid-v7",
			want:  map[string]string{"stable-v6": "stable-v7", "rapid-v6": "rapid-v7"},
		},
		{
			name:  "whitespace handling",
			input: " stable-v6 : stable-v7 , rapid-v6 : rapid-v7 ",
			want:  map[string]string{"stable-v6": "stable-v7", "rapid-v6": "rapid-v7"},
		},
		{name: "missing colon", input: "invalid-entry", wantErr: true},
		{name: "empty source", input: ":stable-v7", wantErr: true},
		{name: "empty target", input: "stable-v6:", wantErr: true},
		{name: "same source and target", input: "stable-v6:stable-v6", wantErr: true},
		{name: "duplicate source", input: "stable-v6:stable-v7,stable-v6:rapid-v7", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.UnleashConfig{ChannelMigrationMap: test.input}
			got, err := cfg.ParseChannelMigrationMap()
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func channelTestChannels() *MockReleaseChannelRepository {
	channels := NewMockReleaseChannelRepository()
	channels.AddChannel("stable-v6", "unleash/unleash-server:6.4.0")
	channels.AddChannel("stable-v7", "unleash/unleash-server:7.6.5")
	return channels
}

func validIntent(name, channel string) *unleash.Config {
	return &unleash.Config{
		Name:                      name,
		ReleaseChannelName:        channel,
		LogLevel:                  "warn",
		DatabasePoolMax:           3,
		DatabasePoolIdleTimeoutMs: 1000,
	}
}

func installTransaction(
	t *testing.T,
	repo *MockUnleashRepository,
	channels *MockReleaseChannelRepository,
	name string,
	phase channelMigrationPhase,
	deadline time.Time,
) {
	t.Helper()
	crd, err := repo.GetCRD(context.Background(), name)
	require.NoError(t, err)
	source, sourceHash, err := loadDesiredStateIntent(crd)
	require.NoError(t, err)
	_, targetHash, err := targetIntent(source, "stable-v7")
	require.NoError(t, err)
	target := channels.channels["stable-v7"]
	transaction := &channelMigrationTransaction{
		SchemaVersion:                channelTransactionSchema,
		ResourceUID:                  crd.UID,
		Phase:                        phase,
		Deadline:                     deadline.UTC(),
		SourceChannel:                source.ReleaseChannelName,
		TargetChannel:                target.Name,
		TargetChannelUID:             target.UID,
		TargetImage:                  target.Image,
		DesiredStateIntentHash:       sourceHash,
		TargetDesiredStateIntentHash: targetHash,
	}
	raw, err := marshalChannelMigrationTransaction(transaction)
	require.NoError(t, err)
	require.NoError(t, repo.PatchAnnotations(
		context.Background(),
		crd,
		map[string]*string{channelMigrationAnnotation: &raw},
	))
}

func setDesiredChannel(t *testing.T, repo *MockUnleashRepository, name, channel string) {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.setIntentLocked(name, validIntent(name, channel))
}

func mustTransaction(t *testing.T, repo *MockUnleashRepository, name string) *channelMigrationTransaction {
	t.Helper()
	transaction, err := transactionFromRepo(repo, name)
	require.NoError(t, err)
	return transaction
}

func transactionFromRepo(repo *MockUnleashRepository, name string) (*channelMigrationTransaction, error) {
	crd, err := repo.GetCRD(context.Background(), name)
	if err != nil {
		return nil, err
	}
	return unmarshalChannelMigrationTransaction(crd.Annotations[channelMigrationAnnotation])
}

func mustInstance(t *testing.T, repo *MockUnleashRepository, name string) *unleash.Instance {
	t.Helper()
	instance, err := repo.Get(context.Background(), name)
	require.NoError(t, err)
	return instance
}

func TestChannelMigrationMarkerPreservesForeignMetadataShape(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-a", "", "stable-v6", true)
	repo.mu.Lock()
	repo.crds["team-a"].OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "unleasherator.nais.io/v1",
		Kind:       "RemoteUnleash",
		Name:       "team-a-remote",
		UID:        "remote-uid",
	}}
	repo.mu.Unlock()

	channels := channelTestChannels()
	installTransaction(t, repo, channels, "team-a", phasePrepared, time.Now().Add(time.Minute))

	crd, err := repo.GetCRD(context.Background(), "team-a")
	require.NoError(t, err)
	require.Len(t, crd.OwnerReferences, 1)
	assert.Equal(t, "team-a-remote", crd.OwnerReferences[0].Name)
}
