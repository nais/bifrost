package migration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errUpdateFailed = errors.New("update failed")
	errCRDNotFound  = errors.New("CRD not found")
	errListFailed   = errors.New("failed to list instances")
)

func newChannelTestConfig(enabled bool, channelMap string, healthTimeout time.Duration) *config.Config {
	return &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:             "unleash",
			ChannelMigrationEnabled:       enabled,
			ChannelMigrationMap:           channelMap,
			ChannelMigrationHealthTimeout: healthTimeout,
		},
	}
}

func newChannelTestReconciler(repo unleash.Repository, channelRepo *MockReleaseChannelRepository, cfg *config.Config) *ChannelReconciler {
	r := NewChannelReconciler(repo, channelRepo, cfg, newTestLogger())
	r.pollInterval = 10 * time.Millisecond
	return r
}

func TestChannelReconciler_Start_Disabled(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test", "", "stable-v5", true)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(false, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_EmptyMap(t *testing.T) {
	repo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	cfg := newChannelTestConfig(true, "", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_InvalidMapFormat(t *testing.T) {
	repo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	cfg := newChannelTestConfig(true, "invalid-no-colon", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_SameSourceAndTarget(t *testing.T) {
	repo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	cfg := newChannelTestConfig(true, "stable-v5:stable-v5", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_SourceChannelNotFound(t *testing.T) {
	repo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")
	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_TargetChannelNotFound(t *testing.T) {
	repo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_NoCandidates(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-alpha", "", "stable-v6", true) // already on target
	repo.AddInstance("team-beta", "6.2.0", "", true)      // custom version

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_Start_DeterministicOrder(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("charlie", "", "stable-v5", false)
	repo.AddInstance("alpha", "", "stable-v5", false)
	repo.AddInstance("bravo", "", "stable-v5", false)

	repo.SetReadyAfterNCalls("alpha", 1)
	repo.SetReadyAfterNCalls("bravo", 1)
	repo.SetReadyAfterNCalls("charlie", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 5*time.Second)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	require.Len(t, repo.updateCalls, 3)
	assert.Equal(t, "alpha", repo.updateCalls[0])
	assert.Equal(t, "bravo", repo.updateCalls[1])
	assert.Equal(t, "charlie", repo.updateCalls[2])
}

func TestChannelReconciler_Start_SkipsOtherChannels(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-v5", "", "stable-v5", false)
	repo.AddInstance("team-v6", "", "stable-v6", true) // already on target
	repo.AddInstance("team-rapid", "", "rapid", true)  // different channel

	repo.SetReadyAfterNCalls("team-v5", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 5*time.Second)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "team-v5", repo.updateCalls[0])
}

func TestChannelReconciler_MigrateInstance_Success(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.SetReadyAfterNCalls("test-instance", 2)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 30*time.Second)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx := context.Background()
	reconciler.Start(ctx)

	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "test-instance", repo.updateCalls[0])

	inst, err := repo.Get(ctx, "test-instance")
	require.NoError(t, err)
	assert.Equal(t, "stable-v6", inst.ReleaseChannelName)
	assert.Empty(t, inst.CustomVersion)
}

func TestChannelReconciler_MigrateInstance_HealthTimeout_Rollback(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	// Never becomes ready after migration -> timeout -> rollback

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 100*time.Millisecond)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx := context.Background()
	reconciler.Start(ctx)

	// 1 for migration, 1 for rollback
	require.Len(t, repo.updateCalls, 2)
	assert.Equal(t, "test-instance", repo.updateCalls[0])
	assert.Equal(t, "test-instance", repo.updateCalls[1])

	inst, err := repo.Get(ctx, "test-instance")
	require.NoError(t, err)
	assert.Equal(t, "stable-v5", inst.ReleaseChannelName)
	assert.Empty(t, inst.CustomVersion)
}

func TestChannelReconciler_SkipsUnhealthyInstances(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", false) // not healthy

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusSkippedUnhealthy, stateVal.(*instanceState).status)
}

func TestChannelReconciler_UpdateFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.updateErr = errUpdateFailed

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusFailed, stateVal.(*instanceState).status)
}

func TestChannelReconciler_GetCRDFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.getCRDErr = errCRDNotFound

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusFailed, stateVal.(*instanceState).status)
}

func TestChannelReconciler_ContextCancellation(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("instance-1", "", "stable-v5", true)
	repo.AddInstance("instance-2", "", "stable-v5", true)
	repo.AddInstance("instance-3", "", "stable-v5", true)

	repo.SetReadyAfterNCalls("instance-1", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 5*time.Second)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		reconciler.Start(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not stop after context cancellation")
	}

	assert.LessOrEqual(t, len(repo.updateCalls), 3)
}

func TestChannelReconciler_MultipleInstances_PartialFailure(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("instance-1", "", "stable-v5", true)
	repo.AddInstance("instance-2", "", "stable-v5", true)
	repo.AddInstance("instance-3", "", "stable-v5", true)

	repo.SetReadyAfterNCalls("instance-1", 1)
	// instance-2 never becomes ready -> timeout -> rollback
	repo.SetReadyAfterNCalls("instance-3", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 100*time.Millisecond)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	state1, _ := reconciler.state.Load("instance-1")
	state2, _ := reconciler.state.Load("instance-2")
	state3, _ := reconciler.state.Load("instance-3")

	assert.Equal(t, statusCompleted, state1.(*instanceState).status)
	assert.Equal(t, statusRollbackFailed, state2.(*instanceState).status)
	assert.Equal(t, statusCompleted, state3.(*instanceState).status)

	ctx := context.Background()
	inst2, err := repo.Get(ctx, "instance-2")
	require.NoError(t, err)
	assert.Equal(t, "stable-v5", inst2.ReleaseChannelName)
}

func TestChannelReconciler_ListError(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.listErr = errListFailed

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)
}

func TestChannelReconciler_MultipleChannelMappings(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-stable-v5", "", "stable-v5", false)
	repo.AddInstance("team-rapid-v5", "", "rapid-v5", false)
	repo.AddInstance("team-already-v6", "", "stable-v6", true)

	repo.SetReadyAfterNCalls("team-rapid-v5", 1)
	repo.SetReadyAfterNCalls("team-stable-v5", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")
	channelRepo.AddChannel("rapid-v5", "unleash/unleash-server:5.14.0")
	channelRepo.AddChannel("rapid-v6", "unleash/unleash-server:6.4.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6,rapid-v5:rapid-v6", 5*time.Second)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx := context.Background()
	reconciler.Start(ctx)

	require.Len(t, repo.updateCalls, 2)
	assert.Equal(t, "team-rapid-v5", repo.updateCalls[0])
	assert.Equal(t, "team-stable-v5", repo.updateCalls[1])

	instRapid, err := repo.Get(ctx, "team-rapid-v5")
	require.NoError(t, err)
	assert.Equal(t, "rapid-v6", instRapid.ReleaseChannelName)

	instStable, err := repo.Get(ctx, "team-stable-v5")
	require.NoError(t, err)
	assert.Equal(t, "stable-v6", instStable.ReleaseChannelName)
}

func TestChannelReconciler_MultipleChannelMappings_PartialRollback(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("team-stable", "", "stable-v5", true)
	repo.AddInstance("team-rapid", "", "rapid-v5", true)

	repo.SetReadyAfterNCalls("team-rapid", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")
	channelRepo.AddChannel("rapid-v5", "unleash/unleash-server:5.14.0")
	channelRepo.AddChannel("rapid-v6", "unleash/unleash-server:6.4.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6,rapid-v5:rapid-v6", 100*time.Millisecond)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx := context.Background()
	reconciler.Start(ctx)

	stateRapid, _ := reconciler.state.Load("team-rapid")
	stateStable, _ := reconciler.state.Load("team-stable")

	assert.Equal(t, statusCompleted, stateRapid.(*instanceState).status)
	assert.Equal(t, statusRollbackFailed, stateStable.(*instanceState).status)

	instStable, err := repo.Get(ctx, "team-stable")
	require.NoError(t, err)
	assert.Equal(t, "stable-v5", instStable.ReleaseChannelName)

	instRapid, err := repo.Get(ctx, "team-rapid")
	require.NoError(t, err)
	assert.Equal(t, "rapid-v6", instRapid.ReleaseChannelName)
}

func TestChannelReconciler_MigrateInstance_GetFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.getErr = errors.New("get failed")

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", testHealthTimeout)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	assert.Empty(t, repo.updateCalls)

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusFailed, stateVal.(*instanceState).status)
}

func TestChannelReconciler_Rollback_GetCRDFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.getCRDErr = errCRDNotFound
	repo.getCRDFailAfterN = 1 // first GetCRD (migration) succeeds, second (rollback) fails

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 100*time.Millisecond)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	require.Len(t, repo.updateCalls, 1)

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusRollbackFailed, stateVal.(*instanceState).status)

	inst, _ := repo.Get(context.Background(), "test-instance")
	assert.Equal(t, "stable-v6", inst.ReleaseChannelName) // stuck on target since rollback failed
}

func TestChannelReconciler_Rollback_UpdateFails(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("test-instance", "", "stable-v5", true)
	repo.updateErr = errUpdateFailed
	repo.updateFailAfterN = 1 // first Update (migration) succeeds, second (rollback) fails

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 100*time.Millisecond)
	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	require.Len(t, repo.updateCalls, 1) // only migration update succeeded

	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	assert.Equal(t, statusRollbackFailed, stateVal.(*instanceState).status)

	inst, _ := repo.Get(context.Background(), "test-instance")
	assert.Equal(t, "stable-v6", inst.ReleaseChannelName) // stuck on target since rollback update failed
}

func TestChannelReconciler_Start_WithMigrationDelay(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("alpha", "", "stable-v5", false)
	repo.AddInstance("bravo", "", "stable-v5", false)

	repo.SetReadyAfterNCalls("alpha", 1)
	repo.SetReadyAfterNCalls("bravo", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 5*time.Second)
	cfg.Unleash.ChannelMigrationDelay = 10 * time.Millisecond

	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	reconciler.Start(context.Background())

	require.Len(t, repo.updateCalls, 2)
	assert.Equal(t, "alpha", repo.updateCalls[0])
	assert.Equal(t, "bravo", repo.updateCalls[1])
}

func TestChannelReconciler_ContextCancellation_DuringDelay(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.AddInstance("alpha", "", "stable-v5", false)
	repo.AddInstance("bravo", "", "stable-v5", false)

	repo.SetReadyAfterNCalls("alpha", 1)
	repo.SetReadyAfterNCalls("bravo", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable-v5", "unleash/unleash-server:5.12.0")
	channelRepo.AddChannel("stable-v6", "unleash/unleash-server:6.3.0")

	cfg := newChannelTestConfig(true, "stable-v5:stable-v6", 5*time.Second)
	cfg.Unleash.ChannelMigrationDelay = 5 * time.Second // long delay to ensure cancellation during it

	reconciler := newChannelTestReconciler(repo, channelRepo, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		reconciler.Start(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not stop during migration delay")
	}

	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "alpha", repo.updateCalls[0])
}

func TestParseChannelMigrationMap(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{
			name:  "empty",
			input: "",
			want:  map[string]string{},
		},
		{
			name:  "single mapping",
			input: "stable-v5:stable-v6",
			want:  map[string]string{"stable-v5": "stable-v6"},
		},
		{
			name:  "multiple mappings",
			input: "stable-v5:stable-v6,rapid-v5:rapid-v6",
			want:  map[string]string{"stable-v5": "stable-v6", "rapid-v5": "rapid-v6"},
		},
		{
			name:  "whitespace handling",
			input: " stable-v5 : stable-v6 , rapid-v5 : rapid-v6 ",
			want:  map[string]string{"stable-v5": "stable-v6", "rapid-v5": "rapid-v6"},
		},
		{
			name:  "trailing comma",
			input: "stable-v5:stable-v6,",
			want:  map[string]string{"stable-v5": "stable-v6"},
		},
		{
			name:    "missing colon",
			input:   "invalid-entry",
			wantErr: true,
		},
		{
			name:    "empty source",
			input:   ":stable-v6",
			wantErr: true,
		},
		{
			name:    "empty target",
			input:   "stable-v5:",
			wantErr: true,
		},
		{
			name:    "same source and target",
			input:   "stable-v5:stable-v5",
			wantErr: true,
		},
		{
			name:    "duplicate source",
			input:   "stable-v5:stable-v6,stable-v5:rapid-v6",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.UnleashConfig{ChannelMigrationMap: tt.input}
			got, err := cfg.ParseChannelMigrationMap()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
