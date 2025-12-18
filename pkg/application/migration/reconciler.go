package migration

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
)

const (
	// defaultPollInterval is the default interval between health checks
	defaultPollInterval = 10 * time.Second
)

// migrationState tracks the original custom version for rollback
type migrationState struct {
	originalCustomVersion string
	status                string // "pending", "in-progress", "completed", "failed", "rolled-back"
}

// UnleashCRDRepository extends unleash.Repository with CRD access needed for migration
type UnleashCRDRepository interface {
	unleash.Repository
	GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error)
}

// Reconciler handles migration of Unleash instances from custom versions to release channels
type Reconciler struct {
	unleashRepo        UnleashCRDRepository
	releaseChannelRepo releasechannel.Repository
	config             *config.Config
	logger             *logrus.Logger

	// pollInterval is the interval between health checks (configurable for testing)
	pollInterval time.Duration

	// state tracks migration progress for each instance
	state sync.Map // map[string]*migrationState

	// mu protects the pending queue during iteration
	mu      sync.Mutex
	pending []string // ordered list of instance names pending migration
}

// NewReconciler creates a new migration reconciler
func NewReconciler(
	unleashRepo UnleashCRDRepository,
	releaseChannelRepo releasechannel.Repository,
	cfg *config.Config,
	logger *logrus.Logger,
) *Reconciler {
	return &Reconciler{
		unleashRepo:        unleashRepo,
		releaseChannelRepo: releaseChannelRepo,
		config:             cfg,
		logger:             logger,
		pollInterval:       defaultPollInterval,
		pending:            make([]string, 0),
	}
}

// Start begins the migration process in the background.
// It will migrate all instances with custom versions to the target release channel.
// The migration is deterministic: instances are processed in alphabetical order.
func (r *Reconciler) Start(ctx context.Context) {
	r.logger.Info("Starting release channel migration reconciler")

	// Validate target channel exists
	targetChannel := r.config.Unleash.MigrationTargetChannel
	if targetChannel == "" {
		r.logger.Error("Migration enabled but no target channel configured (BIFROST_UNLEASH_MIGRATION_TARGET_CHANNEL)")
		return
	}

	channel, err := r.releaseChannelRepo.Get(ctx, targetChannel)
	if err != nil {
		r.logger.WithError(err).Errorf("Migration target channel %q not found", targetChannel)
		return
	}

	r.logger.WithFields(logrus.Fields{
		"targetChannel": targetChannel,
		"channelImage":  channel.Image,
	}).Info("Validated migration target channel")

	// List all instances and filter candidates for migration
	instances, err := r.unleashRepo.List(ctx, false) // include all instances
	if err != nil {
		r.logger.WithError(err).Error("Failed to list instances for migration")
		return
	}

	// Filter to instances with custom versions (candidates for migration)
	// Skip instances that already have a release channel configured
	candidates := make([]*unleash.Instance, 0)
	for _, inst := range instances {
		if inst.CustomVersion != "" && inst.ReleaseChannelName == "" {
			candidates = append(candidates, inst)
		}
	}

	if len(candidates) == 0 {
		r.logger.Info("No instances found with custom versions to migrate")
		return
	}

	// Sort alphabetically for deterministic order
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	// Initialize state and pending queue
	r.mu.Lock()
	for _, inst := range candidates {
		r.state.Store(inst.Name, &migrationState{
			originalCustomVersion: inst.CustomVersion,
			status:                "pending",
		})
		r.pending = append(r.pending, inst.Name)
	}
	r.mu.Unlock()

	r.logger.WithFields(logrus.Fields{
		"candidateCount": len(candidates),
		"targetChannel":  targetChannel,
	}).Info("Found instances to migrate from custom versions to release channel")

	// Process migrations sequentially with delay between each
	migrationDelay := r.config.Unleash.MigrationDelay
	for i, inst := range candidates {
		select {
		case <-ctx.Done():
			r.logCurrentState("Migration interrupted by shutdown")
			return
		default:
			r.migrateInstance(ctx, inst.Name, targetChannel)

			// Add delay between migrations to avoid overwhelming the cluster
			// Skip delay after the last instance
			if i < len(candidates)-1 && migrationDelay > 0 {
				r.logger.WithField("delay", migrationDelay).Debug("Waiting before next migration")
				select {
				case <-ctx.Done():
					r.logCurrentState("Migration interrupted during delay")
					return
				case <-time.After(migrationDelay):
				}
			}
		}
	}

	// Log final summary
	r.logMigrationSummary()
}

// migrateInstance migrates a single instance to the target release channel
func (r *Reconciler) migrateInstance(ctx context.Context, name, targetChannel string) {
	log := r.logger.WithFields(logrus.Fields{
		"instance":      name,
		"targetChannel": targetChannel,
	})

	// Get current state
	stateVal, ok := r.state.Load(name)
	if !ok {
		log.Error("Instance not found in migration state")
		return
	}
	state := stateVal.(*migrationState)

	log.WithField("originalVersion", state.originalCustomVersion).Info("Starting migration")

	// SAFETY CHECK: Verify instance is healthy before attempting migration
	inst, err := r.unleashRepo.Get(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance before migration")
		state.status = "failed"
		return
	}
	if !inst.IsReady {
		log.Warn("Skipping migration: instance is not healthy before migration")
		state.status = "skipped-unhealthy"
		r.removePending(name)
		return
	}

	// Update state to in-progress
	state.status = "in-progress"

	// Get CRD and build updated config
	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for migration")
		state.status = "failed"
		return
	}

	// Build config from existing CRD and update to release channel
	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithReleaseChannel(targetChannel) // This clears CustomVersion
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build migration config")
		state.status = "failed"
		return
	}

	// Apply the update
	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to update instance to release channel")
		state.status = "failed"
		return
	}

	log.Info("Updated instance to release channel, waiting for health check")

	// Wait for instance to become healthy
	if err := r.waitForHealthy(ctx, name); err != nil {
		log.WithError(err).Warn("Instance failed health check after migration, rolling back")
		r.rollback(ctx, name, state.originalCustomVersion)
		state.status = "rolled-back"
		return
	}

	// Migration successful
	state.status = "completed"
	r.removePending(name)

	log.Info("Successfully migrated instance to release channel")
}

// waitForHealthy polls the instance until it becomes ready or timeout
func (r *Reconciler) waitForHealthy(ctx context.Context, name string) error {
	timeout := r.config.Unleash.MigrationHealthTimeout
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return &healthCheckTimeoutError{name: name, timeout: timeout}
			}

			inst, err := r.unleashRepo.Get(ctx, name)
			if err != nil {
				r.logger.WithError(err).WithField("instance", name).Debug("Health check failed to get instance")
				continue
			}

			if inst.IsReady {
				return nil
			}

			r.logger.WithFields(logrus.Fields{
				"instance":  name,
				"isReady":   inst.IsReady,
				"remaining": time.Until(deadline).Round(time.Second),
			}).Debug("Waiting for instance to become healthy")
		}
	}
}

// rollback reverts an instance to its original custom version
func (r *Reconciler) rollback(ctx context.Context, name, originalVersion string) {
	log := r.logger.WithFields(logrus.Fields{
		"instance":        name,
		"originalVersion": originalVersion,
	})

	log.Info("Rolling back instance to original custom version")

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for rollback")
		return
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithCustomVersion(originalVersion) // This clears ReleaseChannelName
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build rollback config")
		return
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to rollback instance")
		return
	}

	// Wait for rollback to restore health
	if err := r.waitForHealthy(ctx, name); err != nil {
		log.WithError(err).Error("CRITICAL: Instance did not recover after rollback - manual intervention required")
		// Don't remove from pending - leave in failed state for visibility
		return
	}

	r.removePending(name)
	log.Info("Successfully rolled back instance to original custom version")
}

// removePending removes an instance from the pending queue
func (r *Reconciler) removePending(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, n := range r.pending {
		if n == name {
			r.pending = append(r.pending[:i], r.pending[i+1:]...)
			break
		}
	}
}

// logCurrentState logs the current migration state for debugging/operator visibility
func (r *Reconciler) logCurrentState(reason string) {
	r.mu.Lock()
	pendingCopy := make([]string, len(r.pending))
	copy(pendingCopy, r.pending)
	r.mu.Unlock()

	states := make(map[string]string)
	r.state.Range(func(key, value any) bool {
		name := key.(string)
		state := value.(*migrationState)
		states[name] = state.status
		return true
	})

	r.logger.WithFields(logrus.Fields{
		"reason":  reason,
		"pending": pendingCopy,
		"states":  states,
	}).Info("Migration reconciler state")
}

// logMigrationSummary logs a summary of the migration results
func (r *Reconciler) logMigrationSummary() {
	var completed, failed, skipped, rolledBack int

	r.state.Range(func(key, value any) bool {
		state := value.(*migrationState)
		switch state.status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "skipped-unhealthy":
			skipped++
		case "rolled-back":
			rolledBack++
		}
		return true
	})

	log := r.logger.WithFields(logrus.Fields{
		"completed":   completed,
		"failed":      failed,
		"skipped":     skipped,
		"rolled_back": rolledBack,
	})

	if failed > 0 || rolledBack > 0 {
		log.Warn("Migration reconciler completed with issues")
	} else {
		log.Info("Migration reconciler completed successfully")
	}
}

// healthCheckTimeoutError is returned when health check times out
type healthCheckTimeoutError struct {
	name    string
	timeout time.Duration
}

func (e *healthCheckTimeoutError) Error() string {
	return "health check timed out for instance " + e.name + " after " + e.timeout.String()
}
