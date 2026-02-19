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

type channelMigrationState struct {
	originalChannel string
	status          migrationStatus
}

type ChannelReconciler struct {
	unleashRepo        UnleashCRDRepository
	releaseChannelRepo releasechannel.Repository
	config             *config.Config
	logger             *logrus.Logger

	pollInterval time.Duration

	state   sync.Map
	mu      sync.Mutex
	pending []string
}

func NewChannelReconciler(
	unleashRepo UnleashCRDRepository,
	releaseChannelRepo releasechannel.Repository,
	cfg *config.Config,
	logger *logrus.Logger,
) *ChannelReconciler {
	return &ChannelReconciler{
		unleashRepo:        unleashRepo,
		releaseChannelRepo: releaseChannelRepo,
		config:             cfg,
		logger:             logger,
		pollInterval:       defaultPollInterval,
		pending:            make([]string, 0),
	}
}

func (r *ChannelReconciler) Start(ctx context.Context) {
	if !r.config.Unleash.ChannelMigrationEnabled {
		r.logger.Debug("Channel migration reconciler called but not enabled, skipping")
		return
	}

	r.logger.Info("Starting channel-to-channel migration reconciler")

	channelMap, err := r.config.Unleash.ParseChannelMigrationMap()
	if err != nil {
		r.logger.WithError(err).Error("Failed to parse channel migration map")
		return
	}

	if len(channelMap) == 0 {
		r.logger.Error("Channel migration enabled but no channel map configured (BIFROST_UNLEASH_CHANNEL_MIGRATION_MAP)")
		return
	}

	for source, target := range channelMap {
		if _, err := r.releaseChannelRepo.Get(ctx, source); err != nil {
			r.logger.WithError(err).Errorf("Channel migration source channel %q not found", source)
			return
		}
		targetCh, err := r.releaseChannelRepo.Get(ctx, target)
		if err != nil {
			r.logger.WithError(err).Errorf("Channel migration target channel %q not found", target)
			return
		}
		r.logger.WithFields(logrus.Fields{
			"sourceChannel": source,
			"targetChannel": target,
			"targetImage":   targetCh.Image,
		}).Info("Validated channel migration mapping")
	}

	instances, err := r.unleashRepo.List(ctx, false)
	if err != nil {
		r.logger.WithError(err).Error("Failed to list instances for channel migration")
		return
	}

	type candidate struct {
		instance      *unleash.Instance
		targetChannel string
	}

	var candidates []candidate
	for _, inst := range instances {
		if target, ok := channelMap[inst.ReleaseChannelName]; ok {
			candidates = append(candidates, candidate{instance: inst, targetChannel: target})
		}
	}

	if len(candidates) == 0 {
		r.logger.Info("No instances found on source channels to migrate")
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].instance.Name < candidates[j].instance.Name
	})

	r.mu.Lock()
	for _, c := range candidates {
		r.state.Store(c.instance.Name, &channelMigrationState{
			originalChannel: c.instance.ReleaseChannelName,
			status:          statusPending,
		})
		r.pending = append(r.pending, c.instance.Name)
	}
	r.mu.Unlock()

	r.logger.WithFields(logrus.Fields{
		"candidateCount": len(candidates),
		"channelMap":     channelMap,
	}).Info("Found instances to migrate between channels")

	migrationDelay := r.config.Unleash.ChannelMigrationDelay
	for i, c := range candidates {
		select {
		case <-ctx.Done():
			r.logCurrentState("Channel migration interrupted by shutdown")
			return
		default:
			r.migrateInstance(ctx, c.instance.Name, c.targetChannel)

			if i < len(candidates)-1 && migrationDelay > 0 {
				r.logger.WithField("delay", migrationDelay).Debug("Waiting before next channel migration")
				select {
				case <-ctx.Done():
					r.logCurrentState("Channel migration interrupted during delay")
					return
				case <-time.After(migrationDelay):
				}
			}
		}
	}

	r.logMigrationSummary()
}

func (r *ChannelReconciler) migrateInstance(ctx context.Context, name, targetChannel string) {
	log := r.logger.WithFields(logrus.Fields{
		"instance":      name,
		"targetChannel": targetChannel,
	})

	stateVal, ok := r.state.Load(name)
	if !ok {
		log.Error("Instance not found in channel migration state")
		return
	}
	state := stateVal.(*channelMigrationState)

	log.WithField("originalChannel", state.originalChannel).Info("Starting channel migration")

	inst, err := r.unleashRepo.Get(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance before channel migration")
		state.status = statusFailed
		r.removePending(name)
		return
	}
	if !inst.IsReady {
		log.Warn("Skipping channel migration: instance is not healthy")
		state.status = statusSkippedUnhealthy
		r.removePending(name)
		return
	}

	state.status = statusInProgress

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for channel migration")
		state.status = statusFailed
		r.removePending(name)
		return
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithReleaseChannel(targetChannel)
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build channel migration config")
		state.status = statusFailed
		r.removePending(name)
		return
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to update instance to target channel")
		state.status = statusFailed
		r.removePending(name)
		return
	}

	log.Info("Updated instance to target channel, waiting for health check")

	if err := waitForHealthy(ctx, r.unleashRepo, r.logger, name, r.config.Unleash.ChannelMigrationHealthTimeout, r.pollInterval); err != nil {
		log.WithError(err).Warn("Instance failed health check after channel migration, rolling back")
		if rbErr := r.rollback(ctx, name, state.originalChannel); rbErr != nil {
			state.status = statusRollbackFailed
		} else {
			state.status = statusRolledBack
		}
		return
	}

	state.status = statusCompleted
	r.removePending(name)

	log.Info("Successfully migrated instance to target channel")
}

func (r *ChannelReconciler) rollback(ctx context.Context, name, originalChannel string) error {
	log := r.logger.WithFields(logrus.Fields{
		"instance":        name,
		"originalChannel": originalChannel,
	})

	log.Info("Rolling back instance to original channel")

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for channel rollback")
		return err
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithReleaseChannel(originalChannel)
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build channel rollback config")
		return err
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to rollback instance to original channel")
		return err
	}

	if err := waitForHealthy(ctx, r.unleashRepo, r.logger, name, r.config.Unleash.ChannelMigrationHealthTimeout, r.pollInterval); err != nil {
		log.WithError(err).Error("CRITICAL: Instance did not recover after channel rollback - manual intervention required")
		return err
	}

	r.removePending(name)
	log.Info("Successfully rolled back instance to original channel")
	return nil
}

func (r *ChannelReconciler) removePending(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, n := range r.pending {
		if n == name {
			r.pending = append(r.pending[:i], r.pending[i+1:]...)
			break
		}
	}
}

func (r *ChannelReconciler) logCurrentState(reason string) {
	r.mu.Lock()
	pendingCopy := make([]string, len(r.pending))
	copy(pendingCopy, r.pending)
	r.mu.Unlock()

	states := make(map[string]string)
	r.state.Range(func(key, value any) bool {
		name := key.(string)
		state := value.(*channelMigrationState)
		states[name] = string(state.status)
		return true
	})

	r.logger.WithFields(logrus.Fields{
		"reason":  reason,
		"pending": pendingCopy,
		"states":  states,
	}).Info("Channel migration reconciler state")
}

func (r *ChannelReconciler) logMigrationSummary() {
	var completed, failed, skipped, rolledBack, rollbackFailed int

	r.state.Range(func(key, value any) bool {
		state := value.(*channelMigrationState)
		switch state.status {
		case statusCompleted:
			completed++
		case statusFailed:
			failed++
		case statusSkippedUnhealthy:
			skipped++
		case statusRolledBack:
			rolledBack++
		case statusRollbackFailed:
			rollbackFailed++
		}
		return true
	})

	log := r.logger.WithFields(logrus.Fields{
		"completed":       completed,
		"failed":          failed,
		"skipped":         skipped,
		"rolled_back":     rolledBack,
		"rollback_failed": rollbackFailed,
	})

	if rollbackFailed > 0 {
		log.Error("Channel migration reconciler completed with rollback failures - manual intervention required")
	} else if failed > 0 || rolledBack > 0 {
		log.Warn("Channel migration reconciler completed with issues")
	} else {
		log.Info("Channel migration reconciler completed successfully")
	}
}
