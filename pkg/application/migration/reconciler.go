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
	"github.com/sirupsen/logrus"
)

// Reconciler handles migration of Unleash instances from custom versions to release channels
type Reconciler struct {
	unleashRepo        unleash.Repository
	releaseChannelRepo releasechannel.Repository
	config             *config.Config
	logger             *logrus.Logger

	pollInterval time.Duration
	state        sync.Map
	pending      *pendingQueue
}

// NewReconciler creates a new migration reconciler
func NewReconciler(
	unleashRepo unleash.Repository,
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
		pending:            newPendingQueue(),
	}
}

// Start begins the migration process in the background.
// It will migrate all instances with custom versions to the target release channel.
// The migration is deterministic: instances are processed in alphabetical order.
func (r *Reconciler) Start(ctx context.Context) {
	if !r.config.Unleash.MigrationEnabled {
		r.logger.Debug("Migration reconciler called but migration is not enabled, skipping")
		return
	}

	r.logger.Info("Starting release channel migration reconciler")

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

	instances, err := r.unleashRepo.List(ctx, false)
	if err != nil {
		r.logger.WithError(err).Error("Failed to list instances for migration")
		return
	}

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

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	for _, inst := range candidates {
		r.state.Store(inst.Name, &instanceState{
			originalValue: inst.CustomVersion,
			status:        statusPending,
		})
		r.pending.add(inst.Name)
	}

	r.logger.WithFields(logrus.Fields{
		"candidateCount": len(candidates),
		"targetChannel":  targetChannel,
	}).Info("Found instances to migrate from custom versions to release channel")

	migrationDelay := r.config.Unleash.MigrationDelay
	for i, inst := range candidates {
		select {
		case <-ctx.Done():
			logCurrentState(r.logger, &r.state, r.pending, "Migration interrupted by shutdown")
			return
		default:
			r.migrateInstance(ctx, inst.Name, targetChannel)

			if i < len(candidates)-1 && migrationDelay > 0 {
				r.logger.WithField("delay", migrationDelay).Debug("Waiting before next migration")
				select {
				case <-ctx.Done():
					logCurrentState(r.logger, &r.state, r.pending, "Migration interrupted during delay")
					return
				case <-time.After(migrationDelay):
				}
			}
		}
	}

	summary := computeSummary(&r.state)
	logSummary(r.logger, summary, "Migration reconciler")
}

func (r *Reconciler) migrateInstance(ctx context.Context, name, targetChannel string) {
	log := r.logger.WithFields(logrus.Fields{
		"instance":      name,
		"targetChannel": targetChannel,
	})

	stateVal, ok := r.state.Load(name)
	if !ok {
		log.Error("Instance not found in migration state")
		return
	}
	state := stateVal.(*instanceState)

	log.WithField("originalVersion", state.originalValue).Info("Starting migration")

	inst, err := r.unleashRepo.Get(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance before migration")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}
	if !inst.IsReady {
		log.Warn("Skipping migration: instance is not healthy before migration")
		state.status = statusSkippedUnhealthy
		r.pending.remove(name)
		return
	}

	state.status = statusInProgress

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for migration")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithReleaseChannel(targetChannel)
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build migration config")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to update instance to release channel")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	log.Info("Updated instance to release channel, waiting for health check")

	if err := waitForHealthy(ctx, r.unleashRepo, r.logger, name, r.config.Unleash.MigrationHealthTimeout, r.pollInterval); err != nil {
		log.WithError(err).Warn("Instance failed health check after migration, rolling back")
		if rbErr := r.rollback(ctx, name, state.originalValue); rbErr != nil {
			state.status = statusRollbackFailed
		} else {
			state.status = statusRolledBack
		}
		return
	}

	state.status = statusCompleted
	r.pending.remove(name)

	log.Info("Successfully migrated instance to release channel")
}

func (r *Reconciler) rollback(ctx context.Context, name, originalVersion string) error {
	log := r.logger.WithFields(logrus.Fields{
		"instance":        name,
		"originalVersion": originalVersion,
	})

	log.Info("Rolling back instance to original custom version")

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for rollback")
		return err
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithCustomVersion(originalVersion)
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build rollback config")
		return err
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to rollback instance")
		return err
	}

	if err := waitForHealthy(ctx, r.unleashRepo, r.logger, name, r.config.Unleash.MigrationHealthTimeout, r.pollInterval); err != nil {
		log.WithError(err).Error("CRITICAL: Instance did not recover after rollback - manual intervention required")
		return err
	}

	r.pending.remove(name)
	log.Info("Successfully rolled back instance to original custom version")
	return nil
}
