package reconciler

import (
	"context"
	"fmt"
	"sort"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// adoptFleet runs one durable, single-flight legacy canonicalization. A
// checkpoint is created before the target write and is only released after the
// target CR reports current-generation health.
func (r *UnleashReconciler) adoptFleet(ctx context.Context) {
	ns, err := instanceNamespace(r.config)
	if err != nil {
		r.blockAdoption(ctx, nil, nil, "the instance namespace is invalid; correct BIFROST_UNLEASH_INSTANCE_NAMESPACE")
		return
	}
	list := &unleashv1.UnleashList{}
	if err := r.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		r.logger.WithError(err).Warn("Failed to list Unleash instances for legacy adoption")
		return
	}
	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].Name == list.Items[j].Name {
			return string(list.Items[i].UID) < string(list.Items[j].UID)
		}
		return list.Items[i].Name < list.Items[j].Name
	})

	for i := range list.Items {
		if list.Items[i].Annotations[kubernetes.AnnotationChannelMigration] != "" {
			setAdoptionCheckpointState(adoptionStateWaitingMigration)
			r.logger.WithField("instance", list.Items[i].Name).
				Info("Yielding legacy adoption while a channel-migration transaction exists")
			return
		}
	}

	configMap, checkpoint, exists, err := r.getAdoptionCheckpoint(ctx, ns)
	if err != nil {
		r.blockAdoption(ctx, configMap, checkpoint, "the adoption checkpoint ConfigMap is invalid or unreadable; restore it from a known-good state before retrying")
		r.logger.WithError(err).Error("Refusing legacy adoption because the durable checkpoint is unsafe")
		return
	}
	if !exists {
		if err := legacyAdoptionMarkers(list); err != nil {
			setAdoptionCheckpointState(adoptionStateBlocked)
			adoptionsTotal.WithLabelValues(adoptionError).Inc()
			r.logger.WithError(err).Error("Refusing legacy adoption because its checkpoint ConfigMap was deleted or replaced")
			return
		}
		r.adoptNext(ctx, ns, list, nil, nil)
		return
	}

	switch checkpoint.Phase {
	case adoptionPrepared:
		r.resumePreparedAdoption(ctx, list, configMap, checkpoint)
	case adoptionTargetWritten:
		r.verifyPendingAdoption(ctx, list, configMap, checkpoint)
	case adoptionCheckpointVerified:
		if err := validateVerifiedCheckpoint(list, configMap, checkpoint); err != nil {
			r.blockAdoption(ctx, configMap, checkpoint, err.Error())
			return
		}
		setAdoptionCheckpointState(adoptionStateVerified)
		adoptionPendingVerification.Set(0)
		r.adoptNext(ctx, ns, list, configMap, checkpoint)
	case adoptionFailed:
		setAdoptionCheckpointState(adoptionStateFailed)
		adoptionPendingVerification.Set(0)
		r.logger.WithField("instance", checkpoint.ResourceName).WithField("reason", checkpoint.Failure).
			Error("Legacy adoption is in durable manual-recovery state; no further candidates will be changed")
	default:
		r.blockAdoption(ctx, configMap, checkpoint, "the adoption checkpoint has an unknown phase; inspect the ConfigMap before retrying")
	}
}

// legacyAdoptionMarkers makes deletion of the ConfigMap fail closed. A CR-side
// marker means Bifrost had already started a transaction, so a missing
// checkpoint must not be interpreted as permission to start the next one.
func legacyAdoptionMarkers(list *unleashv1.UnleashList) error {
	for i := range list.Items {
		if list.Items[i].Annotations[kubernetes.AnnotationAdoption] == "" {
			continue
		}
		if _, _, err := adoptionRecord(&list.Items[i]); err != nil {
			return fmt.Errorf("instance %q has an invalid adoption marker: %w", list.Items[i].Name, err)
		}
		return fmt.Errorf("instance %q has an adoption marker but ConfigMap %q is absent", list.Items[i].Name, adoptionCheckpointName)
	}
	return nil
}

func (r *UnleashReconciler) adoptNext(ctx context.Context, ns string, list *unleashv1.UnleashList, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint) {
	for i := range list.Items {
		crd := &list.Items[i]
		log := r.logger.WithField("instance", crd.Name)
		if !r.adoptable(crd, log) {
			continue
		}

		plan, err := planLegacyAdoption(crd, r.config)
		if err != nil {
			adoptionsTotal.WithLabelValues(adoptionRefused).Inc()
			log.WithError(err).Error("Refusing unsafe legacy CR shape; remove the named manual field or leave this CR unmanaged")
			continue
		}
		log = log.WithField("canonicalized_fields", plan.canonicalized)
		if r.dryRun {
			setAdoptionCheckpointState(adoptionStatePreview)
			adoptionsTotal.WithLabelValues(adoptionPreviewed).Inc()
			log.Info("Would create a legacy-adoption checkpoint and canonicalize this CR (dry-run)")
			return
		}

		newCheckpoint := newAdoptionCheckpoint(plan, ns, crd.Name, crd.UID, crd.Generation)
		if configMap == nil {
			configMap, err = r.createAdoptionCheckpoint(ctx, ns, newCheckpoint)
			if err != nil {
				adoptionsTotal.WithLabelValues(adoptionError).Inc()
				log.WithError(err).Error("Failed to create durable legacy-adoption checkpoint")
				return
			}
		} else {
			if err := r.updateAdoptionCheckpoint(ctx, configMap, newCheckpoint); err != nil {
				adoptionsTotal.WithLabelValues(adoptionError).Inc()
				log.WithError(err).Error("Failed to start the next durable legacy-adoption checkpoint")
				return
			}
		}
		if err := r.writeAdoptionTarget(ctx, crd, configMap, newCheckpoint, plan); err != nil {
			r.blockAdoption(ctx, configMap, newCheckpoint, "the target write did not complete; inspect the source CR and checkpoint before retrying")
			log.WithError(err).Error("Failed to canonicalize legacy CR after checkpoint creation")
			return
		}
		setAdoptionCheckpointState(adoptionStatePending)
		adoptionsTotal.WithLabelValues(adoptionAdopted).Inc()
		log.Info("Canonicalized legacy CR; waiting for current-generation health before continuing")
		return
	}
	setAdoptionCheckpointState(adoptionStateIdle)
	adoptionPendingVerification.Set(0)
}

func (r *UnleashReconciler) resumePreparedAdoption(ctx context.Context, list *unleashv1.UnleashList, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint) {
	crd := findCheckpointCRD(list, checkpoint)
	if crd == nil || crd.DeletionTimestamp != nil {
		r.blockAdoption(ctx, configMap, checkpoint, "the source CR was deleted or is deleting before canonicalization completed")
		return
	}

	if raw := crd.Annotations[kubernetes.AnnotationAdoption]; raw != "" {
		record, _, err := adoptionRecord(crd)
		if err != nil || record.Phase != kubernetes.AdoptionPending ||
			checkpointMatchesRecord(checkpoint, configMap, record) != nil ||
			!kubernetes.MatchesAdoptionTarget(record, crd) {
			r.blockAdoption(ctx, configMap, checkpoint, "the source CR marker or target hashes no longer match the prepared checkpoint")
			return
		}
		if r.dryRun {
			adoptionsTotal.WithLabelValues(adoptionPreviewed).Inc()
			r.logger.WithField("instance", crd.Name).Info("Would recover target-written legacy adoption checkpoint (dry-run)")
			return
		}
		checkpoint.Phase = adoptionTargetWritten
		if err := r.updateAdoptionCheckpoint(ctx, configMap, checkpoint); err != nil {
			adoptionsTotal.WithLabelValues(adoptionError).Inc()
			r.logger.WithField("instance", crd.Name).WithError(err).Error("Failed to recover target-written adoption checkpoint")
		}
		return
	}
	if crd.Generation != checkpoint.SourceGeneration {
		r.blockAdoption(ctx, configMap, checkpoint, "the source CR generation changed after the checkpoint was captured")
		return
	}

	plan, err := planLegacyAdoption(crd, r.config)
	if err != nil || !checkpointMatchesPlan(checkpoint, plan, crd.UID) {
		r.blockAdoption(ctx, configMap, checkpoint, "the source CR no longer matches the source snapshot or canonical target stored in the checkpoint")
		return
	}
	if r.dryRun {
		adoptionsTotal.WithLabelValues(adoptionPreviewed).Inc()
		r.logger.WithField("instance", crd.Name).Info("Would recover prepared legacy adoption checkpoint (dry-run)")
		return
	}
	if err := r.writeAdoptionTarget(ctx, crd, configMap, checkpoint, plan); err != nil {
		r.blockAdoption(ctx, configMap, checkpoint, "the recovered target write failed; inspect the CR and checkpoint before retrying")
		r.logger.WithField("instance", crd.Name).WithError(err).Error("Failed to recover prepared legacy adoption")
	}
}

func (r *UnleashReconciler) writeAdoptionTarget(ctx context.Context, crd *unleashv1.Unleash, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint, plan *adoptionPlan) error {
	record, err := kubernetes.NewAdoptionRecord(
		crd, configMap.Name, checkpoint.ID, configMap.UID, plan.target.Spec, plan.targetIntent,
	)
	if err != nil {
		return fmt.Errorf("create CR adoption marker: %w", err)
	}
	raw, err := kubernetes.MarshalAdoption(record)
	if err != nil {
		return fmt.Errorf("marshal CR adoption marker: %w", err)
	}

	base := crd.DeepCopy()
	crd.Spec = plan.target.Spec
	applyManagedMetadata(crd, &plan.target)
	if crd.Annotations == nil {
		crd.Annotations = make(map[string]string)
	}
	crd.Annotations[kubernetes.AnnotationAdoption] = raw
	if !kubernetes.AnnotationsFit(crd.Annotations) {
		return fmt.Errorf("adoption marker exceeds Kubernetes annotation size limit")
	}
	if err := r.client.Patch(ctx, crd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch canonical legacy CR: %w", err)
	}

	checkpoint.Phase = adoptionTargetWritten
	if err := r.updateAdoptionCheckpoint(ctx, configMap, checkpoint); err != nil {
		return err
	}
	return nil
}

func (r *UnleashReconciler) verifyPendingAdoption(ctx context.Context, list *unleashv1.UnleashList, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint) {
	crd, record, err := checkpointTarget(list, configMap, checkpoint, kubernetes.AdoptionPending)
	if err != nil {
		r.blockAdoption(ctx, configMap, checkpoint, err.Error())
		return
	}
	if !kubernetes.IsReadyForCurrentGeneration(crd) {
		setAdoptionCheckpointState(adoptionStatePending)
		adoptionPendingVerification.Set(1)
		r.logger.WithField("instance", crd.Name).Info("Waiting for canonicalized CR to report current-generation Reconciled=True and Connected=True")
		return
	}
	if r.dryRun {
		adoptionsTotal.WithLabelValues(adoptionPreviewed).Inc()
		r.logger.WithField("instance", crd.Name).Info("Would verify healthy legacy-adoption checkpoint (dry-run)")
		return
	}

	base := crd.DeepCopy()
	record.Phase = kubernetes.AdoptionVerified
	raw, err := kubernetes.MarshalAdoption(record)
	if err != nil {
		r.blockAdoption(ctx, configMap, checkpoint, "the healthy CR marker could not be serialized")
		return
	}
	crd.Annotations[kubernetes.AnnotationAdoption] = raw
	if err := r.client.Patch(ctx, crd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		adoptionsTotal.WithLabelValues(adoptionError).Inc()
		r.logger.WithField("instance", crd.Name).WithError(err).Error("Failed to mark healthy legacy CR as verified")
		return
	}

	now := metav1.Now()
	checkpoint.Phase = adoptionCheckpointVerified
	checkpoint.VerifiedAt = &now
	if err := r.updateAdoptionCheckpoint(ctx, configMap, checkpoint); err != nil {
		adoptionsTotal.WithLabelValues(adoptionError).Inc()
		r.logger.WithField("instance", crd.Name).WithError(err).Error("Failed to persist verified legacy-adoption checkpoint")
		return
	}
	setAdoptionCheckpointState(adoptionStateVerified)
	adoptionPendingVerification.Set(0)
	adoptionsTotal.WithLabelValues(adoptionVerified).Inc()
	r.logger.WithField("instance", crd.Name).Info("Verified legacy adoption checkpoint after current-generation health")
}

func checkpointTarget(list *unleashv1.UnleashList, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint, phase kubernetes.AdoptionPhase) (*unleashv1.Unleash, kubernetes.AdoptionRecord, error) {
	crd := findCheckpointCRD(list, checkpoint)
	if crd == nil {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint source CR was deleted")
	}
	if crd.DeletionTimestamp != nil {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint source CR is deleting")
	}
	if crd.Namespace != checkpoint.Namespace {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint source CR namespace changed")
	}
	record, present, err := adoptionRecord(crd)
	if err != nil || !present {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint CR marker is missing or invalid")
	}
	if record.Phase != phase {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint CR marker is in phase %q, expected %q", record.Phase, phase)
	}
	if err := checkpointMatchesRecord(checkpoint, configMap, record); err != nil {
		return nil, kubernetes.AdoptionRecord{}, err
	}
	if crd.UID != checkpoint.ResourceUID || !kubernetes.MatchesAdoptionTarget(record, crd) {
		return nil, kubernetes.AdoptionRecord{}, fmt.Errorf("the checkpoint CR UID, canonical spec hash, or desired-state hash changed")
	}
	return crd, record, nil
}

func validateVerifiedCheckpoint(list *unleashv1.UnleashList, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint) error {
	crd := findCheckpointCRD(list, checkpoint)
	if crd == nil {
		// Deletion after a verified canonicalization is not a partial
		// transaction. The next candidate may use the durable checkpoint.
		return nil
	}
	_, _, err := checkpointTarget(list, configMap, checkpoint, kubernetes.AdoptionVerified)
	return err
}

func findCheckpointCRD(list *unleashv1.UnleashList, checkpoint *adoptionCheckpoint) *unleashv1.Unleash {
	for i := range list.Items {
		crd := &list.Items[i]
		if crd.Name == checkpoint.ResourceName {
			return crd
		}
	}
	return nil
}

func adoptionRecord(crd *unleashv1.Unleash) (kubernetes.AdoptionRecord, bool, error) {
	raw := crd.Annotations[kubernetes.AnnotationAdoption]
	if raw == "" {
		return kubernetes.AdoptionRecord{}, false, nil
	}
	record, err := kubernetes.UnmarshalAdoption(raw)
	if err != nil {
		return kubernetes.AdoptionRecord{}, false, err
	}
	return record, true, nil
}

func (r *UnleashReconciler) blockAdoption(ctx context.Context, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint, reason string) {
	setAdoptionCheckpointState(adoptionStateBlocked)
	adoptionPendingVerification.Set(0)
	adoptionsTotal.WithLabelValues(adoptionError).Inc()
	if r.dryRun || configMap == nil || checkpoint == nil {
		r.logger.WithField("reason", reason).Error("Legacy adoption is blocked; no CR will be changed")
		return
	}
	checkpoint.Phase = adoptionFailed
	checkpoint.Failure = reason
	if err := r.updateAdoptionCheckpoint(ctx, configMap, checkpoint); err != nil {
		r.logger.WithError(err).WithField("reason", reason).
			Error("Legacy adoption is blocked and the durable failure state could not be written")
		return
	}
	setAdoptionCheckpointState(adoptionStateFailed)
	r.logger.WithField("reason", reason).Error("Legacy adoption is blocked in durable manual-recovery state")
}

func (r *UnleashReconciler) adoptable(crd *unleashv1.Unleash, log *logrus.Entry) bool {
	if crd.DeletionTimestamp != nil || crd.Annotations[kubernetes.AnnotationDesiredState] != "" ||
		crd.Annotations[kubernetes.AnnotationAdoption] != "" || crd.Annotations[kubernetes.AnnotationChannelMigration] != "" {
		return false
	}
	if _, managed := crd.Labels[kubernetes.LabelManagedBy]; managed {
		return false
	}
	if crd.Labels[kubernetes.LabelAdopt] != kubernetes.AdoptOptIn {
		log.Info("Skipping legacy CR without explicit bifrost.nais.io/adopt=true approval")
		return false
	}
	return true
}
