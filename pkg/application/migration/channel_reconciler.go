package migration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type annotationPatcher interface {
	PatchAnnotations(ctx context.Context, crd *unleashv1.Unleash, changes map[string]*string) error
}

type transactionState struct {
	crd         *unleashv1.Unleash
	transaction *channelMigrationTransaction
	intent      *unleash.Config
	intentHash  string
}

type channelCandidate struct {
	sourceChannel string
	name          string
	targetChannel string
}

// ChannelReconciler handles migration of Unleash instances between release channels.
type ChannelReconciler struct {
	unleashRepo        unleash.Repository
	releaseChannelRepo releasechannel.Repository
	config             *config.Config
	logger             *logrus.Logger

	pollInterval time.Duration
	retryDelay   time.Duration
	now          func() time.Time
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
		retryDelay:         defaultTransactionRetryDelay,
		now:                time.Now,
	}
}

// Start recovers every persisted transaction before admitting a bounded number
// of new migrations. Recovery runs even when new admission is disabled.
func (r *ChannelReconciler) Start(ctx context.Context) {
	crds, err := r.unleashRepo.ListCRDs(ctx, false)
	if err != nil {
		r.logger.WithError(err).Error("Failed to list instances for channel migration recovery")
		return
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].Name < crds[j].Name })

	withTransaction := make(map[string]bool)
	for i := range crds {
		raw := crds[i].GetAnnotations()[channelMigrationAnnotation]
		if raw == "" {
			continue
		}
		withTransaction[crds[i].Name] = true
		if _, err := unmarshalChannelMigrationTransaction(raw); err != nil {
			recordChannelMigrationEvent(channelEventInvalidTransaction)
			r.logger.WithError(err).WithField("instance", crds[i].Name).
				Error("Invalid persisted channel migration transaction; manual intervention required")
			continue
		}

		recordChannelMigrationEvent(channelEventResumed)
		r.resumeTransaction(ctx, crds[i].Name)
		if ctx.Err() != nil {
			return
		}
	}

	if !r.config.Unleash.ChannelMigrationEnabled {
		r.logger.Debug("Channel migration admission disabled; persisted transaction recovery completed")
		return
	}

	channelMap, err := r.config.Unleash.ParseChannelMigrationMap()
	if err != nil {
		r.logger.WithError(err).Error("Failed to parse channel migration map")
		return
	}
	if len(channelMap) == 0 {
		r.logger.Error("Channel migration enabled but no channel map configured (BIFROST_UNLEASH_CHANNEL_MIGRATION_MAP)")
		return
	}
	if r.config.Unleash.ChannelMigrationMaxCandidates <= 0 {
		r.logger.Info("Channel migration admission capped at zero; no new transactions admitted")
		return
	}
	if err := r.validateChannelMap(ctx, channelMap); err != nil {
		r.logger.WithError(err).Error("Channel migration map validation failed")
		return
	}

	candidates := r.newCandidates(crds, withTransaction, channelMap)
	maxCandidates := r.config.Unleash.ChannelMigrationMaxCandidates
	if len(candidates) > maxCandidates {
		recordChannelMigrationEvent(channelEventCanaryLimited)
		candidates = candidates[:maxCandidates]
	}

	for i, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}

		instance, err := r.unleashRepo.Get(ctx, candidate.name)
		if err != nil {
			r.logger.WithError(err).WithField("instance", candidate.name).
				Warn("Could not verify channel migration candidate health")
			continue
		}
		if !instance.IsReady {
			recordChannelMigrationEvent(channelEventSkippedUnhealthy)
			r.logger.WithField("instance", candidate.name).
				Warn("Skipping channel migration candidate because it is not healthy")
			continue
		}

		if err := r.admitTransaction(ctx, candidate.name, candidate.sourceChannel, candidate.targetChannel); err != nil {
			r.logTransactionError(candidate.name, "Failed to admit channel migration transaction", err)
			continue
		}
		r.resumeTransaction(ctx, candidate.name)

		if i < len(candidates)-1 && r.config.Unleash.ChannelMigrationDelay > 0 {
			if !sleepWithContext(ctx, r.config.Unleash.ChannelMigrationDelay) {
				return
			}
		}
	}
}

func (r *ChannelReconciler) validateChannelMap(ctx context.Context, channelMap map[string]string) error {
	for source, target := range channelMap {
		if _, err := r.releaseChannelRepo.Get(ctx, source); err != nil {
			return fmt.Errorf("source channel %q: %w", source, err)
		}
		targetChannel, err := r.releaseChannelRepo.Get(ctx, target)
		if err != nil {
			return fmt.Errorf("target channel %q: %w", target, err)
		}
		if targetChannel.UID == "" || targetChannel.Image == "" {
			return fmt.Errorf("target channel %q has no UID or image to pin", target)
		}
	}
	return nil
}

func (r *ChannelReconciler) newCandidates(
	crds []unleashv1.Unleash,
	withTransaction map[string]bool,
	channelMap map[string]string,
) []channelCandidate {
	candidates := make([]channelCandidate, 0)
	for i := range crds {
		crd := &crds[i]
		if withTransaction[crd.Name] {
			continue
		}
		if !kubernetes.IsManagedByBifrost(crd) {
			recordChannelMigrationEvent(channelEventSkippedUnmanaged)
			continue
		}
		cfg, _, err := loadDesiredStateIntent(crd)
		if err != nil {
			recordChannelMigrationEvent(channelEventSkippedNoIntent)
			r.logger.WithError(err).WithField("instance", crd.Name).
				Warn("Skipping channel migration candidate with invalid desired-state intent")
			continue
		}
		target, ok := channelMap[cfg.ReleaseChannelName]
		if !ok || cfg.CustomVersion != "" {
			continue
		}
		candidates = append(candidates, channelCandidate{
			name:          crd.Name,
			sourceChannel: cfg.ReleaseChannelName,
			targetChannel: target,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	return candidates
}

func (r *ChannelReconciler) admitTransaction(ctx context.Context, name, sourceChannelName, targetChannelName string) error {
	patcher, ok := r.unleashRepo.(annotationPatcher)
	if !ok {
		return errors.New("unleash repository does not support annotation CAS")
	}

	for attempt := 0; attempt < transactionAnnotationRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		crd, err := r.unleashRepo.GetCRD(ctx, name)
		if err != nil {
			return err
		}
		if crd.GetAnnotations()[channelMigrationAnnotation] != "" {
			return nil
		}
		if !kubernetes.IsManagedByBifrost(crd) {
			return ownershipError("instance is no longer managed by Bifrost")
		}
		if crd.UID == "" {
			return ownershipError("instance has no Kubernetes UID")
		}

		sourceIntent, sourceHash, err := loadDesiredStateIntent(crd)
		if err != nil {
			return ownershipError(err.Error())
		}
		if sourceIntent.ReleaseChannelName != sourceChannelName || sourceIntent.CustomVersion != "" {
			return ownershipError("current desired-state intent is no longer on the configured source channel")
		}

		targetChannel, err := r.releaseChannelRepo.Get(ctx, targetChannelName)
		if err != nil {
			return err
		}
		if targetChannel.UID == "" || targetChannel.Image == "" {
			return fmt.Errorf("target channel %q has no UID or image to pin", targetChannelName)
		}
		_, targetHash, err := targetIntent(sourceIntent, targetChannelName)
		if err != nil {
			return err
		}

		transaction := &channelMigrationTransaction{
			SchemaVersion:                channelTransactionSchema,
			ResourceUID:                  crd.UID,
			Phase:                        phasePrepared,
			Deadline:                     r.now().UTC().Add(r.config.Unleash.ChannelMigrationHealthTimeout),
			SourceChannel:                sourceIntent.ReleaseChannelName,
			TargetChannel:                targetChannelName,
			TargetChannelUID:             targetChannel.UID,
			TargetImage:                  targetChannel.Image,
			DesiredStateIntentHash:       sourceHash,
			TargetDesiredStateIntentHash: targetHash,
		}
		raw, err := marshalChannelMigrationTransaction(transaction)
		if err != nil {
			return err
		}
		if err := patcher.PatchAnnotations(ctx, crd, map[string]*string{channelMigrationAnnotation: &raw}); err != nil {
			if apierrors.IsConflict(err) {
				recordChannelMigrationEvent(channelEventConflictRetry)
				if sleepWithContext(ctx, r.retryDelay) {
					continue
				}
				return ctx.Err()
			}
			return err
		}

		recordChannelMigrationEvent(channelEventAdmitted)
		return nil
	}
	return errors.New("channel migration admission conflicts exhausted")
}

func (r *ChannelReconciler) resumeTransaction(ctx context.Context, name string) {
	for step := 0; step < 8 && ctx.Err() == nil; step++ {
		state, err := r.readTransaction(ctx, name)
		if err != nil {
			r.logTransactionError(name, "Cannot resume channel migration transaction", err)
			return
		}

		switch state.transaction.Phase {
		case phasePrepared:
			err = r.ensureTargetWritten(ctx, name)
		case phaseTargetWritten:
			err = r.ensureTargetHealthy(ctx, name)
		case phaseRollbackPending:
			err = r.ensureRollbackWritten(ctx, name)
		case phaseRollbackWritten:
			err = r.ensureRollbackHealthy(ctx, name)
		case phaseCompleted, phaseRolledBack, phaseManualRecovery:
			return
		default:
			err = ownershipError("transaction phase is not supported")
		}
		if err != nil {
			r.logTransactionError(name, "Channel migration transaction stopped", err)
			return
		}
	}
}

func (r *ChannelReconciler) ensureTargetWritten(ctx context.Context, name string) error {
	for attempt := 0; attempt < targetWriteRetries; attempt++ {
		state, err := r.readTransaction(ctx, name)
		if err != nil {
			return err
		}
		transaction := state.transaction
		if transaction.Phase != phasePrepared {
			return nil
		}
		if !r.now().Before(transaction.Deadline) {
			return r.handleTargetFailure(ctx, name, failureTransactionDeadline)
		}
		if err := r.checkPinnedTarget(ctx, transaction); err != nil {
			if errors.Is(err, errTargetChannelChanged) {
				return r.handleTargetFailure(ctx, name, failureTargetChannelChanged)
			}
			return err
		}

		switch {
		case state.intentHash == transaction.TargetDesiredStateIntentHash &&
			state.intent.ReleaseChannelName == transaction.TargetChannel:
			return r.transition(ctx, name, []channelMigrationPhase{phasePrepared}, phaseTargetWritten,
				[]string{transaction.TargetDesiredStateIntentHash}, time.Time{}, "")
		case state.intentHash != transaction.DesiredStateIntentHash ||
			state.intent.ReleaseChannelName != transaction.SourceChannel:
			recordChannelMigrationEvent(channelEventOwnershipChanged)
			return ownershipError(failureIntentOwnershipChanged)
		}

		instance, err := r.unleashRepo.Get(ctx, name)
		if err != nil {
			return err
		}
		if !instance.IsReady {
			return r.markManualRecovery(ctx, name, failureSourceUnhealthy)
		}

		targetConfig, targetHash, err := targetIntent(state.intent, transaction.TargetChannel)
		if err != nil {
			return err
		}
		if targetHash != transaction.TargetDesiredStateIntentHash {
			return ownershipError("target desired-state intent no longer matches the transaction")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = r.unleashRepo.Update(ctx, targetConfig, unleash.UpdateOptions{
			ExpectedResourceVersion: state.crd.ResourceVersion,
		})
		if err == nil {
			return r.transition(ctx, name, []channelMigrationPhase{phasePrepared}, phaseTargetWritten,
				[]string{transaction.TargetDesiredStateIntentHash}, time.Time{}, "")
		}
		if apierrors.IsConflict(err) {
			recordChannelMigrationEvent(channelEventConflictRetry)
			if sleepWithContext(ctx, r.retryDelay) {
				continue
			}
			return ctx.Err()
		}
		return r.handleTargetFailure(ctx, name, failureTargetWrite)
	}
	return r.handleTargetFailure(ctx, name, failureTargetWrite)
}

func (r *ChannelReconciler) ensureTargetHealthy(ctx context.Context, name string) error {
	state, err := r.readTransaction(ctx, name)
	if err != nil {
		return err
	}
	err = r.waitForTransactionHealthy(
		ctx,
		name,
		phaseTargetWritten,
		state.transaction.TargetDesiredStateIntentHash,
		state.transaction.Deadline,
		true,
	)
	switch {
	case err == nil:
		if err := r.checkPinnedTarget(ctx, state.transaction); err != nil {
			if errors.Is(err, errTargetChannelChanged) {
				return r.handleTargetFailure(ctx, name, failureTargetChannelChanged)
			}
			return err
		}
		if err := r.transition(ctx, name, []channelMigrationPhase{phaseTargetWritten}, phaseCompleted,
			[]string{state.transaction.TargetDesiredStateIntentHash}, time.Time{}, ""); err != nil {
			return err
		}
		recordChannelMigrationEvent(channelEventCompleted)
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errTargetChannelChanged):
		return r.handleTargetFailure(ctx, name, failureTargetChannelChanged)
	case errors.Is(err, errTransactionTimeout):
		return r.handleTargetFailure(ctx, name, failureTargetTimeout)
	default:
		return err
	}
}

func (r *ChannelReconciler) handleTargetFailure(ctx context.Context, name, reason string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !r.config.Unleash.ChannelMigrationRollbackSafe {
		return r.markManualRecovery(ctx, name, reason)
	}
	return r.transition(ctx, name, []channelMigrationPhase{phasePrepared, phaseTargetWritten}, phaseRollbackPending,
		nil, r.now().UTC().Add(r.config.Unleash.ChannelMigrationHealthTimeout), reason)
}

func (r *ChannelReconciler) ensureRollbackWritten(ctx context.Context, name string) error {
	for attempt := 0; attempt < targetWriteRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, err := r.readTransaction(ctx, name)
		if err != nil {
			return err
		}
		transaction := state.transaction
		if transaction.Phase != phaseRollbackPending {
			return nil
		}
		if !r.now().Before(transaction.Deadline) {
			return r.markManualRecovery(ctx, name, failureRollbackTimeout)
		}

		switch {
		case state.intentHash == transaction.DesiredStateIntentHash &&
			state.intent.ReleaseChannelName == transaction.SourceChannel:
			return r.transition(ctx, name, []channelMigrationPhase{phaseRollbackPending}, phaseRollbackWritten,
				[]string{transaction.DesiredStateIntentHash}, time.Time{}, "")
		case state.intentHash != transaction.TargetDesiredStateIntentHash ||
			state.intent.ReleaseChannelName != transaction.TargetChannel:
			recordChannelMigrationEvent(channelEventOwnershipChanged)
			return ownershipError(failureIntentOwnershipChanged)
		}

		sourceConfig := *state.intent
		sourceConfig.ReleaseChannelName = transaction.SourceChannel
		sourceHash, err := desiredStateIntentHash(&sourceConfig)
		if err != nil {
			return err
		}
		if sourceHash != transaction.DesiredStateIntentHash {
			return ownershipError("source desired-state intent no longer matches the transaction")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = r.unleashRepo.Update(ctx, &sourceConfig, unleash.UpdateOptions{
			ExpectedResourceVersion: state.crd.ResourceVersion,
		})
		if err == nil {
			return r.transition(ctx, name, []channelMigrationPhase{phaseRollbackPending}, phaseRollbackWritten,
				[]string{transaction.DesiredStateIntentHash}, time.Time{}, "")
		}
		if apierrors.IsConflict(err) {
			recordChannelMigrationEvent(channelEventConflictRetry)
			if sleepWithContext(ctx, r.retryDelay) {
				continue
			}
			return ctx.Err()
		}
		return r.markManualRecovery(ctx, name, failureRollbackWrite)
	}
	return r.markManualRecovery(ctx, name, failureRollbackWrite)
}

func (r *ChannelReconciler) ensureRollbackHealthy(ctx context.Context, name string) error {
	state, err := r.readTransaction(ctx, name)
	if err != nil {
		return err
	}
	err = r.waitForTransactionHealthy(
		ctx,
		name,
		phaseRollbackWritten,
		state.transaction.DesiredStateIntentHash,
		state.transaction.Deadline,
		false,
	)
	switch {
	case err == nil:
		if err := r.transition(ctx, name, []channelMigrationPhase{phaseRollbackWritten}, phaseRolledBack,
			[]string{state.transaction.DesiredStateIntentHash}, time.Time{}, ""); err != nil {
			return err
		}
		recordChannelMigrationEvent(channelEventRolledBack)
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errTransactionTimeout):
		return r.markManualRecovery(ctx, name, failureRollbackTimeout)
	default:
		return err
	}
}

func (r *ChannelReconciler) waitForTransactionHealthy(
	ctx context.Context,
	name string,
	expectedPhase channelMigrationPhase,
	expectedHash string,
	deadline time.Time,
	validateTarget bool,
) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		state, err := r.readTransaction(ctx, name)
		if err == nil {
			if state.transaction.Phase != expectedPhase ||
				state.intentHash != expectedHash ||
				!intentMatchesPhase(state) {
				recordChannelMigrationEvent(channelEventOwnershipChanged)
				return ownershipError(failureIntentOwnershipChanged)
			}
			if validateTarget {
				if err := r.checkPinnedTarget(ctx, state.transaction); err != nil {
					if errors.Is(err, errTargetChannelChanged) {
						return err
					}
				} else if instance, getErr := r.unleashRepo.Get(ctx, name); getErr == nil &&
					instance.IsReady &&
					instance.ChannelNameFromStatus == state.transaction.TargetChannel &&
					instance.ResolvedImage == state.transaction.TargetImage {
					return nil
				}
			} else if instance, getErr := r.unleashRepo.Get(ctx, name); getErr == nil &&
				instance.IsReady &&
				instance.ChannelNameFromStatus == state.transaction.SourceChannel {
				return nil
			}
		} else if isOwnershipError(err) {
			recordChannelMigrationEvent(channelEventOwnershipChanged)
			return err
		}

		if !r.now().Before(deadline) {
			return errTransactionTimeout
		}

		wait := r.pollInterval
		if remaining := deadline.Sub(r.now()); remaining < wait {
			wait = remaining
		}
		if !sleepWithContext(ctx, wait) {
			return ctx.Err()
		}
	}
}

func intentMatchesPhase(state *transactionState) bool {
	switch state.transaction.Phase {
	case phasePrepared:
		return (state.intentHash == state.transaction.DesiredStateIntentHash &&
			state.intent.ReleaseChannelName == state.transaction.SourceChannel) ||
			(state.intentHash == state.transaction.TargetDesiredStateIntentHash &&
				state.intent.ReleaseChannelName == state.transaction.TargetChannel)
	case phaseTargetWritten:
		return state.intentHash == state.transaction.TargetDesiredStateIntentHash &&
			state.intent.ReleaseChannelName == state.transaction.TargetChannel
	case phaseRollbackPending:
		return (state.intentHash == state.transaction.DesiredStateIntentHash &&
			state.intent.ReleaseChannelName == state.transaction.SourceChannel) ||
			(state.intentHash == state.transaction.TargetDesiredStateIntentHash &&
				state.intent.ReleaseChannelName == state.transaction.TargetChannel)
	case phaseRollbackWritten, phaseRolledBack:
		return state.intentHash == state.transaction.DesiredStateIntentHash &&
			state.intent.ReleaseChannelName == state.transaction.SourceChannel
	case phaseCompleted:
		return state.intentHash == state.transaction.TargetDesiredStateIntentHash &&
			state.intent.ReleaseChannelName == state.transaction.TargetChannel
	case phaseManualRecovery:
		return state.intentHash == state.transaction.DesiredStateIntentHash ||
			state.intentHash == state.transaction.TargetDesiredStateIntentHash
	default:
		return false
	}
}

func (r *ChannelReconciler) checkPinnedTarget(ctx context.Context, transaction *channelMigrationTransaction) error {
	channel, err := r.releaseChannelRepo.Get(ctx, transaction.TargetChannel)
	if err != nil {
		return err
	}
	if channel.UID != transaction.TargetChannelUID || channel.Image != transaction.TargetImage {
		return errTargetChannelChanged
	}
	return nil
}

func (r *ChannelReconciler) markManualRecovery(ctx context.Context, name, reason string) error {
	if err := r.transition(ctx, name,
		[]channelMigrationPhase{phasePrepared, phaseTargetWritten, phaseRollbackPending, phaseRollbackWritten},
		phaseManualRecovery, nil, time.Time{}, reason); err != nil {
		return err
	}
	recordChannelMigrationEvent(channelEventManualRecovery)
	return nil
}

func (r *ChannelReconciler) transition(
	ctx context.Context,
	name string,
	from []channelMigrationPhase,
	to channelMigrationPhase,
	allowedHashes []string,
	deadline time.Time,
	failureReason string,
) error {
	patcher, ok := r.unleashRepo.(annotationPatcher)
	if !ok {
		return errors.New("unleash repository does not support annotation CAS")
	}

	for attempt := 0; attempt < transactionAnnotationRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, err := r.readTransaction(ctx, name)
		if err != nil {
			return err
		}
		if state.transaction.Phase == to {
			return nil
		}
		if !phaseAllowed(state.transaction.Phase, from) {
			return ownershipError(fmt.Sprintf("transaction phase changed from expected state to %q", state.transaction.Phase))
		}
		if !hashAllowed(state.intentHash, state.transaction, allowedHashes) || !intentMatchesPhase(state) {
			recordChannelMigrationEvent(channelEventOwnershipChanged)
			return ownershipError(failureIntentOwnershipChanged)
		}

		updated := *state.transaction
		updated.Phase = to
		if !deadline.IsZero() {
			updated.Deadline = deadline
		}
		updated.FailureReason = failureReason
		raw, err := marshalChannelMigrationTransaction(&updated)
		if err != nil {
			return err
		}
		if err := patcher.PatchAnnotations(ctx, state.crd, map[string]*string{channelMigrationAnnotation: &raw}); err != nil {
			if apierrors.IsConflict(err) {
				recordChannelMigrationEvent(channelEventConflictRetry)
				if sleepWithContext(ctx, r.retryDelay) {
					continue
				}
				return ctx.Err()
			}
			return err
		}
		return nil
	}
	return errors.New("channel migration transaction annotation conflicts exhausted")
}

func hashAllowed(hash string, transaction *channelMigrationTransaction, allowed []string) bool {
	if allowed == nil {
		return hash == transaction.DesiredStateIntentHash || hash == transaction.TargetDesiredStateIntentHash
	}
	for _, allowedHash := range allowed {
		if hash == allowedHash {
			return true
		}
	}
	return false
}

func phaseAllowed(phase channelMigrationPhase, allowed []channelMigrationPhase) bool {
	for _, candidate := range allowed {
		if phase == candidate {
			return true
		}
	}
	return false
}

func (r *ChannelReconciler) readTransaction(ctx context.Context, name string) (*transactionState, error) {
	crd, err := r.unleashRepo.GetCRD(ctx, name)
	if err != nil {
		return nil, err
	}
	if !kubernetes.IsManagedByBifrost(crd) {
		return nil, ownershipError("instance is no longer managed by Bifrost")
	}
	raw := crd.GetAnnotations()[channelMigrationAnnotation]
	if raw == "" {
		return nil, ownershipError("channel migration transaction annotation is missing")
	}
	transaction, err := unmarshalChannelMigrationTransaction(raw)
	if err != nil {
		return nil, ownershipError(err.Error())
	}
	if crd.UID == "" || crd.UID != transaction.ResourceUID {
		return nil, ownershipError("channel migration transaction resource UID does not match the live instance")
	}
	intent, hash, err := loadDesiredStateIntent(crd)
	if err != nil {
		return nil, ownershipError(err.Error())
	}
	return &transactionState{
		crd:         crd,
		transaction: transaction,
		intent:      intent,
		intentHash:  hash,
	}, nil
}

func (r *ChannelReconciler) logTransactionError(name, message string, err error) {
	entry := r.logger.WithError(err).WithField("instance", name)
	if isOwnershipError(err) {
		recordChannelMigrationEvent(channelEventOwnershipChanged)
		entry.Error(message + "; ownership changed or persisted state is invalid")
		return
	}
	entry.Error(message)
}

func sleepWithContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
