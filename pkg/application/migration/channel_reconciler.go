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

// ChannelReconciler handles migration of Unleash instances between release channels
type ChannelReconciler struct {
	unleashRepo        unleash.Repository
	releaseChannelRepo releasechannel.Repository
	config             *config.Config
	logger             *logrus.Logger

	pollInterval time.Duration
	state        sync.Map
	pending      *pendingQueue
}

func NewChannelReconciler(
	unleashRepo unleash.Repository,
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
		pending:            newPendingQueue(),
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

	for _, c := range candidates {
		r.state.Store(c.instance.Name, &instanceState{
			originalValue: c.instance.ReleaseChannelName,
			status:        statusPending,
		})
		r.pending.add(c.instance.Name)
	}

	r.logger.WithFields(logrus.Fields{
		"candidateCount": len(candidates),
		"channelMap":     channelMap,
	}).Info("Found instances to migrate between channels")

	migrationDelay := r.config.Unleash.ChannelMigrationDelay
	for i, c := range candidates {
		select {
		case <-ctx.Done():
			logCurrentState(r.logger, &r.state, r.pending, "Channel migration interrupted by shutdown")
			return
		default:
			r.migrateInstance(ctx, c.instance.Name, c.targetChannel)

			if i < len(candidates)-1 && migrationDelay > 0 {
				r.logger.WithField("delay", migrationDelay).Debug("Waiting before next channel migration")
				select {
				case <-ctx.Done():
					logCurrentState(r.logger, &r.state, r.pending, "Channel migration interrupted during delay")
					return
				case <-time.After(migrationDelay):
				}
			}
		}
	}

	summary := computeSummary(&r.state)
	logSummary(r.logger, summary, "Channel migration reconciler")
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
	state := stateVal.(*instanceState)

	log.WithField("originalChannel", state.originalValue).Info("Starting channel migration")

	inst, err := r.unleashRepo.Get(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance before channel migration")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}
	if !inst.IsReady {
		log.Warn("Skipping channel migration: instance is not healthy")
		state.status = statusSkippedUnhealthy
		r.pending.remove(name)
		return
	}

	state.status = statusInProgress

	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		log.WithError(err).Error("Failed to get instance CRD for channel migration")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	builder := kubernetes.LoadConfigFromCRD(crd)
	builder.WithReleaseChannel(targetChannel)
	cfg, err := builder.Build()
	if err != nil {
		log.WithError(err).Error("Failed to build channel migration config")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	if err := r.unleashRepo.Update(ctx, cfg); err != nil {
		log.WithError(err).Error("Failed to update instance to target channel")
		state.status = statusFailed
		r.pending.remove(name)
		return
	}

	log.Info("Updated instance to target channel, waiting for health check")

	if err := waitForHealthy(ctx, r.unleashRepo, r.logger, name, r.config.Unleash.ChannelMigrationHealthTimeout, r.pollInterval); err != nil {
		log.WithError(err).Warn("Instance failed health check after channel migration, rolling back")
		if rbErr := r.rollback(ctx, name, state.originalValue); rbErr != nil {
			state.status = statusRollbackFailed
		} else {
			state.status = statusRolledBack
		}
		return
	}

	state.status = statusCompleted
	r.pending.remove(name)

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

	r.pending.remove(name)
	log.Info("Successfully rolled back instance to original channel")
	return nil
}
