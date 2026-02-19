package migration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MockUnleashCRDRepository implements UnleashCRDRepository for testing
type MockUnleashCRDRepository struct {
	mu               sync.Mutex
	instances        map[string]*unleash.Instance
	crds             map[string]*unleashv1.Unleash
	listErr          error
	getErr           error
	getCRDErr        error
	getCRDCallCount  int
	getCRDFailAfterN int // If > 0, GetCRD succeeds for first N calls, then returns getCRDErr
	updateErr        error
	updateFailAfterN int // If > 0, Update succeeds for first N calls, then returns updateErr
	updateCalls      []string
	readyAfter       map[string]int // instance name -> number of Get calls before becoming ready
	getCounts        map[string]int // track Get calls per instance
}

func NewMockUnleashCRDRepository() *MockUnleashCRDRepository {
	return &MockUnleashCRDRepository{
		instances:  make(map[string]*unleash.Instance),
		crds:       make(map[string]*unleashv1.Unleash),
		readyAfter: make(map[string]int),
		getCounts:  make(map[string]int),
	}
}

func (m *MockUnleashCRDRepository) List(ctx context.Context, excludeChannelInstances bool) ([]*unleash.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listErr != nil {
		return nil, m.listErr
	}

	var result []*unleash.Instance
	for _, inst := range m.instances {
		if excludeChannelInstances && inst.ReleaseChannelName != "" {
			continue
		}
		result = append(result, inst)
	}
	return result, nil
}

func (m *MockUnleashCRDRepository) ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error) {
	return nil, nil // Not used in migration tests
}

func (m *MockUnleashCRDRepository) Get(ctx context.Context, name string) (*unleash.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.getErr != nil {
		return nil, m.getErr
	}

	inst, ok := m.instances[name]
	if !ok {
		return nil, errors.New("instance not found")
	}

	// Track get calls for health check simulation
	m.getCounts[name]++

	// Simulate instance becoming ready after N calls
	if readyAfter, exists := m.readyAfter[name]; exists {
		if m.getCounts[name] >= readyAfter {
			inst.IsReady = true
		}
	}

	return inst, nil
}

func (m *MockUnleashCRDRepository) GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.getCRDCallCount++

	if m.getCRDErr != nil {
		if m.getCRDFailAfterN == 0 || m.getCRDCallCount > m.getCRDFailAfterN {
			return nil, m.getCRDErr
		}
	}

	crd, ok := m.crds[name]
	if !ok {
		return nil, errors.New("CRD not found")
	}
	return crd, nil
}

func (m *MockUnleashCRDRepository) Create(ctx context.Context, cfg *unleash.Config) error {
	return nil // Not used in migration tests
}

func (m *MockUnleashCRDRepository) Update(ctx context.Context, cfg *unleash.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.updateErr != nil {
		if m.updateFailAfterN == 0 || len(m.updateCalls) >= m.updateFailAfterN {
			return m.updateErr
		}
	}

	m.updateCalls = append(m.updateCalls, cfg.Name)

	// Update the instance
	if inst, ok := m.instances[cfg.Name]; ok {
		inst.CustomVersion = cfg.CustomVersion
		inst.ReleaseChannelName = cfg.ReleaseChannelName
		inst.IsReady = false // Reset ready state after update
	}

	// Reset get count for this instance so readyAfter works relative to post-update state
	m.getCounts[cfg.Name] = 0

	// Update the CRD
	if crd, ok := m.crds[cfg.Name]; ok {
		if cfg.CustomVersion != "" {
			crd.Spec.CustomImage = "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:" + cfg.CustomVersion
			crd.Spec.ReleaseChannel.Name = ""
		} else if cfg.ReleaseChannelName != "" {
			crd.Spec.CustomImage = ""
			crd.Spec.ReleaseChannel.Name = cfg.ReleaseChannelName
		}
	}

	return nil
}

func (m *MockUnleashCRDRepository) Delete(ctx context.Context, name string) error {
	return nil // Not used in migration tests
}

func (m *MockUnleashCRDRepository) AddInstance(name, customVersion, releaseChannel string, isReady bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.instances[name] = &unleash.Instance{
		Name:               name,
		Namespace:          "unleash",
		CustomVersion:      customVersion,
		ReleaseChannelName: releaseChannel,
		IsReady:            isReady,
		CreatedAt:          time.Now(),
	}

	customImage := ""
	if customVersion != "" {
		customImage = "europe-north1-docker.pkg.dev/nais-io/nais/images/unleash-v4:" + customVersion
	}

	m.crds[name] = &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "unleash",
		},
		Spec: unleashv1.UnleashSpec{
			CustomImage: customImage,
			ReleaseChannel: unleashv1.UnleashReleaseChannelConfig{
				Name: releaseChannel,
			},
		},
	}
}

func (m *MockUnleashCRDRepository) SetReadyAfterNCalls(name string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyAfter[name] = n
}

// MockReleaseChannelRepository implements releasechannel.Repository for testing
type MockReleaseChannelRepository struct {
	channels map[string]*releasechannel.Channel
	getErr   error
}

func NewMockReleaseChannelRepository() *MockReleaseChannelRepository {
	return &MockReleaseChannelRepository{
		channels: make(map[string]*releasechannel.Channel),
	}
}

func (m *MockReleaseChannelRepository) List(ctx context.Context) ([]*releasechannel.Channel, error) {
	var result []*releasechannel.Channel
	for _, ch := range m.channels {
		result = append(result, ch)
	}
	return result, nil
}

func (m *MockReleaseChannelRepository) Get(ctx context.Context, name string) (*releasechannel.Channel, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}

	ch, ok := m.channels[name]
	if !ok {
		return nil, errors.New("channel not found")
	}
	return ch, nil
}

func (m *MockReleaseChannelRepository) AddChannel(name, image string) {
	m.channels[name] = &releasechannel.Channel{
		Name:      name,
		Image:     image,
		CreatedAt: time.Now(),
	}
}

// testHealthTimeout is a short timeout for tests to avoid long waits
const testHealthTimeout = 200 * time.Millisecond

func newTestConfig(migrationEnabled bool, targetChannel string, healthTimeout time.Duration) *config.Config {
	return &config.Config{
		Unleash: config.UnleashConfig{
			InstanceNamespace:      "unleash",
			MigrationEnabled:       migrationEnabled,
			MigrationTargetChannel: targetChannel,
			MigrationHealthTimeout: healthTimeout,
		},
	}
}

func newTestLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // Reduce log noise in tests
	return logger
}

// newTestReconciler creates a reconciler with a fast poll interval for testing
func newTestReconciler(repo UnleashCRDRepository, channelRepo releasechannel.Repository, cfg *config.Config, logger *logrus.Logger) *Reconciler {
	r := NewReconciler(repo, channelRepo, cfg, logger)
	r.pollInterval = 10 * time.Millisecond // Fast polling for tests
	return r
}

func TestReconciler_Start_MigrationDisabled(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(false, "stable", testHealthTimeout) // Migration disabled
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls when migration is disabled
	assert.Empty(t, repo.updateCalls)
}

func TestReconciler_Start_NoTargetChannel(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	channelRepo := NewMockReleaseChannelRepository()
	cfg := newTestConfig(true, "", testHealthTimeout) // Empty target channel
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls
	assert.Empty(t, repo.updateCalls)
}

func TestReconciler_Start_TargetChannelNotFound(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	channelRepo := NewMockReleaseChannelRepository()
	// Don't add any channels - target won't be found
	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls
	assert.Empty(t, repo.updateCalls)
}

func TestReconciler_Start_NoMigrationCandidates(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	// Add instances that are already on release channels or have no custom version
	repo.AddInstance("team-alpha", "", "stable", true)
	repo.AddInstance("team-beta", "", "rapid", true)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls (no candidates)
	assert.Empty(t, repo.updateCalls)
}

func TestReconciler_Start_DeterministicOrder(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	// Add instances in non-alphabetical order
	repo.AddInstance("charlie", "6.2.0", "", false)
	repo.AddInstance("alpha", "6.1.0", "", false)
	repo.AddInstance("bravo", "6.0.0", "", false)

	// Make them become ready immediately
	repo.SetReadyAfterNCalls("alpha", 1)
	repo.SetReadyAfterNCalls("bravo", 1)
	repo.SetReadyAfterNCalls("charlie", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", 5*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should have processed in alphabetical order: alpha, bravo, charlie
	// Each instance gets 2 updates: 1 for migration, but we only track update calls
	// The update calls should be in alphabetical order
	require.Len(t, repo.updateCalls, 3)
	assert.Equal(t, "alpha", repo.updateCalls[0])
	assert.Equal(t, "bravo", repo.updateCalls[1])
	assert.Equal(t, "charlie", repo.updateCalls[2])
}

func TestReconciler_Start_SkipsAlreadyMigrated(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	// Mix of custom version and release channel instances
	repo.AddInstance("team-custom", "6.2.0", "", false)
	repo.AddInstance("team-channel", "", "stable", true) // Already on channel

	repo.SetReadyAfterNCalls("team-custom", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", 5*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should only update team-custom, not team-channel
	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "team-custom", repo.updateCalls[0])
}

func TestReconciler_MigrateInstance_Success(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true) // Must be healthy to migrate
	repo.SetReadyAfterNCalls("test-instance", 2)         // Stays ready (2nd Get after update)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", 30*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should have updated the instance
	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "test-instance", repo.updateCalls[0])

	// Instance should now have release channel set
	inst, err := repo.Get(ctx, "test-instance")
	require.NoError(t, err)
	assert.Equal(t, "stable", inst.ReleaseChannelName)
	assert.Empty(t, inst.CustomVersion)
}

func TestReconciler_MigrateInstance_HealthTimeout_Rollback(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true) // Must be healthy to migrate
	// After update, instance becomes unhealthy and never recovers - will timeout and rollback

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	// Use a very short timeout for testing
	cfg := newTestConfig(true, "stable", 100*time.Millisecond)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should have 2 update calls: 1 for migration, 1 for rollback
	require.Len(t, repo.updateCalls, 2)
	assert.Equal(t, "test-instance", repo.updateCalls[0]) // Migration
	assert.Equal(t, "test-instance", repo.updateCalls[1]) // Rollback

	// Instance should be rolled back to original custom version
	inst, err := repo.Get(ctx, "test-instance")
	require.NoError(t, err)
	assert.Equal(t, "6.2.0", inst.CustomVersion)
	assert.Empty(t, inst.ReleaseChannelName)
}

func TestReconciler_MigrateInstance_UpdateFails(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true) // Must be healthy to attempt migration
	repo.updateErr = errors.New("update failed")

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// State should be "failed" - check via internal state
	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	state := stateVal.(*migrationState)
	assert.Equal(t, statusFailed, state.status)
}

func TestReconciler_MigrateInstance_GetCRDFails(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true) // Must be healthy to attempt migration
	repo.getCRDErr = errors.New("CRD not found")

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls
	assert.Empty(t, repo.updateCalls)

	// State should be "failed"
	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	state := stateVal.(*migrationState)
	assert.Equal(t, statusFailed, state.status)
}

func TestReconciler_MigrateInstance_SkipsUnhealthyInstances(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", false) // Instance is NOT healthy

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls - instance was skipped
	assert.Empty(t, repo.updateCalls)

	// State should be "skipped-unhealthy"
	stateVal, ok := reconciler.state.Load("test-instance")
	require.True(t, ok)
	state := stateVal.(*migrationState)
	assert.Equal(t, statusSkippedUnhealthy, state.status)
}

func TestReconciler_Start_ContextCancellation(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	// Add multiple instances - must be healthy to attempt migration
	repo.AddInstance("instance-1", "6.1.0", "", true)
	repo.AddInstance("instance-2", "6.2.0", "", true)
	repo.AddInstance("instance-3", "6.3.0", "", true)

	// Make first instance become ready after update
	repo.SetReadyAfterNCalls("instance-1", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.4.0")

	cfg := newTestConfig(true, "stable", 5*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Start in goroutine and cancel after first migration
	done := make(chan struct{})
	go func() {
		reconciler.Start(ctx)
		close(done)
	}()

	// Give time for first migration to start
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait for reconciler to finish
	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("reconciler did not stop after context cancellation")
	}

	// Should have processed at most the first instance
	// (might have processed more depending on timing)
	assert.LessOrEqual(t, len(repo.updateCalls), 3)
}

func TestReconciler_WaitForHealthy_ImmediateReady(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", true) // Already ready

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", 5*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()

	// Call waitForHealthy directly
	err := reconciler.waitForHealthy(ctx, "test-instance")
	assert.NoError(t, err)
}

func TestReconciler_WaitForHealthy_EventuallyReady(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", false)
	repo.SetReadyAfterNCalls("test-instance", 3) // Ready after 3 calls

	channelRepo := NewMockReleaseChannelRepository()

	cfg := newTestConfig(true, "stable", 5*time.Second)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()

	err := reconciler.waitForHealthy(ctx, "test-instance")
	assert.NoError(t, err)

	// Should have made multiple Get calls
	assert.GreaterOrEqual(t, repo.getCounts["test-instance"], 3)
}

func TestReconciler_WaitForHealthy_ContextCancelled(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "6.2.0", "", false)
	// Never becomes ready

	channelRepo := NewMockReleaseChannelRepository()

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := reconciler.waitForHealthy(ctx, "test-instance")
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

func TestReconciler_Rollback_Success(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "", "stable", true)
	repo.SetReadyAfterNCalls("test-instance", 1)

	channelRepo := NewMockReleaseChannelRepository()

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()

	err := reconciler.rollback(ctx, "test-instance", "6.2.0")
	assert.NoError(t, err)

	require.Len(t, repo.updateCalls, 1)
	assert.Equal(t, "test-instance", repo.updateCalls[0])

	// Instance should have custom version restored
	inst, err := repo.Get(ctx, "test-instance")
	require.NoError(t, err)
	assert.Equal(t, "6.2.0", inst.CustomVersion)
	assert.Empty(t, inst.ReleaseChannelName)
}

func TestReconciler_Rollback_UpdateFails(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.AddInstance("test-instance", "", "stable", false)
	repo.updateErr = errors.New("update failed")

	channelRepo := NewMockReleaseChannelRepository()

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()

	err := reconciler.rollback(ctx, "test-instance", "6.2.0")
	assert.Error(t, err)

	assert.Empty(t, repo.updateCalls)
}

func TestReconciler_LogCurrentState(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	channelRepo := NewMockReleaseChannelRepository()
	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	// Add some state
	reconciler.state.Store("instance-1", &migrationState{
		originalCustomVersion: "6.1.0",
		status:                statusCompleted,
	})
	reconciler.state.Store("instance-2", &migrationState{
		originalCustomVersion: "6.2.0",
		status:                statusInProgress,
	})
	reconciler.pending = []string{"instance-3", "instance-4"}

	// Should not panic
	reconciler.logCurrentState("test reason")
}

func TestReconciler_RemovePending(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	channelRepo := NewMockReleaseChannelRepository()
	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)
	reconciler.pending = []string{"a", "b", "c", "d"}

	reconciler.removePending("b")
	assert.Equal(t, []string{"a", "c", "d"}, reconciler.pending)

	reconciler.removePending("a")
	assert.Equal(t, []string{"c", "d"}, reconciler.pending)

	reconciler.removePending("d")
	assert.Equal(t, []string{"c"}, reconciler.pending)

	reconciler.removePending("nonexistent")
	assert.Equal(t, []string{"c"}, reconciler.pending)
}

func TestReconciler_MultipleInstances_PartialFailure(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	// All instances start ready (for pre-migration health check)
	repo.AddInstance("instance-1", "6.1.0", "", true)
	repo.AddInstance("instance-2", "6.2.0", "", true)
	repo.AddInstance("instance-3", "6.3.0", "", true)

	// Instance 1 becomes ready immediately after migration
	repo.SetReadyAfterNCalls("instance-1", 1)
	// Instance 2 never becomes ready after migration (will timeout and rollback)
	// Instance 3 becomes ready immediately after migration
	repo.SetReadyAfterNCalls("instance-3", 1)

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.4.0")

	// Short timeout for faster test
	cfg := newTestConfig(true, "stable", 100*time.Millisecond)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Check final states
	state1, _ := reconciler.state.Load("instance-1")
	state2, _ := reconciler.state.Load("instance-2")
	state3, _ := reconciler.state.Load("instance-3")

	assert.Equal(t, statusCompleted, state1.(*migrationState).status)
	assert.Equal(t, statusRollbackFailed, state2.(*migrationState).status)
	assert.Equal(t, statusCompleted, state3.(*migrationState).status)

	// Instance 2 should be rolled back to original version
	inst2, err := repo.Get(ctx, "instance-2")
	require.NoError(t, err)
	assert.Equal(t, "6.2.0", inst2.CustomVersion)
	assert.Empty(t, inst2.ReleaseChannelName)
}

func TestReconciler_ListError(t *testing.T) {
	repo := NewMockUnleashCRDRepository()
	repo.listErr = errors.New("failed to list instances")

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.AddChannel("stable", "unleash/unleash-server:6.3.0")

	cfg := newTestConfig(true, "stable", testHealthTimeout)
	logger := newTestLogger()

	reconciler := newTestReconciler(repo, channelRepo, cfg, logger)

	ctx := context.Background()
	reconciler.Start(ctx)

	// Should not have made any update calls
	assert.Empty(t, repo.updateCalls)
}

func TestHealthCheckTimeoutError(t *testing.T) {
	err := &healthCheckTimeoutError{
		name:    "test-instance",
		timeout: 5 * time.Minute,
	}

	assert.Contains(t, err.Error(), "test-instance")
	assert.Contains(t, err.Error(), "5m0s")
}
