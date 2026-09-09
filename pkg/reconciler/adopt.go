package reconciler

import (
	"context"
	"fmt"
	"sort"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const adoptionPendingMarker = "pending"

// adoptFleet progresses at most one CR-local adoption marker per sweep. It
// always cleans up a pending marker, while AutoAdopt only permits new writes.
func (r *UnleashReconciler) adoptFleet(ctx context.Context) {
	ns, err := instanceNamespace(r.config)
	if err != nil {
		recordAdoptionEvent(adoptionEventError)
		r.logger.WithError(err).Error("Cannot reconcile legacy adoption")
		return
	}

	list := &unleashv1.UnleashList{}
	if err := r.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		recordAdoptionEvent(adoptionEventError)
		r.logger.WithError(err).Warn("Failed to list Unleash instances for legacy adoption")
		return
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Name < list.Items[j].Name
	})

	adoptionRemaining.Set(float64(adoptionRemainingCount(list)))

	for i := range list.Items {
		if list.Items[i].Annotations[kubernetes.AnnotationChannelMigration] != "" {
			recordAdoptionEvent(adoptionEventYieldedToMigration)
			r.logger.WithField("instance", list.Items[i].Name).
				Info("Yielding legacy adoption while a channel-migration transaction exists")
			return
		}
	}

	pending, err := pendingAdoptions(list)
	adoptionPending.Set(float64(len(pending)))
	if err != nil {
		recordAdoptionEvent(adoptionEventBlockedUnknownMarker)
		r.logger.WithError(err).Error("Refusing legacy adoption because an adoption marker is invalid")
		return
	}

	if len(pending) > 1 {
		recordAdoptionEvent(adoptionEventBlockedMultiplePending)
		r.logger.WithField("pending_count", len(pending)).
			Error("Refusing legacy adoption because more than one CR is pending")
		return
	}
	if len(pending) == 1 {
		if !kubernetes.IsManagedByBifrost(pending[0]) {
			recordAdoptionEvent(adoptionEventBlockedUnknownMarker)
			r.logger.WithField("instance", pending[0].Name).
				Error("Refusing legacy adoption because the pending marker is not on a Bifrost-managed CR")
			return
		}
		r.cleanupPendingAdoption(ctx, pending[0])
		return
	}
	if !r.config.Reconciler.AutoAdopt {
		return
	}

	r.adoptNext(ctx, list)
}

func adoptionRemainingCount(list *unleashv1.UnleashList) int {
	remaining := 0
	for i := range list.Items {
		crd := &list.Items[i]
		if !kubernetes.IsManagedByBifrost(crd) {
			continue
		}
		_, present, err := recordedIntent(crd)
		if err != nil || !present {
			remaining++
		}
	}
	return remaining
}

func pendingAdoptions(list *unleashv1.UnleashList) ([]*unleashv1.Unleash, error) {
	pending := make([]*unleashv1.Unleash, 0, 1)
	for i := range list.Items {
		crd := &list.Items[i]
		switch crd.Annotations[kubernetes.AnnotationAdoption] {
		case "":
		case adoptionPendingMarker:
			pending = append(pending, crd)
		default:
			return pending, fmt.Errorf("instance %q has an unsupported adoption marker", crd.Name)
		}
	}
	return pending, nil
}

func (r *UnleashReconciler) cleanupPendingAdoption(ctx context.Context, crd *unleashv1.Unleash) {
	if crd.DeletionTimestamp != nil {
		recordAdoptionEvent(adoptionEventWaitingDeletion)
		r.logger.WithField("instance", crd.Name).Info("Waiting for pending legacy adoption CR deletion")
		return
	}
	if !kubernetes.IsReadyForCurrentGeneration(crd) {
		recordAdoptionEvent(adoptionEventWaitingHealthy)
		r.logger.WithField("instance", crd.Name).
			Info("Waiting for pending legacy adoption CR to report current-generation Reconciled=True and Connected=True")
		return
	}
	if r.dryRun {
		recordAdoptionEvent(adoptionEventDryRun)
		r.logger.WithField("instance", crd.Name).Info("Would remove healthy legacy-adoption pending marker (dry-run)")
		return
	}

	base := crd.DeepCopy()
	delete(crd.Annotations, kubernetes.AnnotationAdoption)
	if err := r.client.Patch(ctx, crd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		recordAdoptionEvent(adoptionEventError)
		r.logger.WithField("instance", crd.Name).WithError(err).
			Error("Failed to remove healthy legacy-adoption pending marker")
		return
	}

	adoptionPending.Set(0)
	recordAdoptionEvent(adoptionEventVerified)
	r.logger.WithField("instance", crd.Name).
		Info("Removed healthy legacy-adoption pending marker")
}

func (r *UnleashReconciler) adoptNext(ctx context.Context, list *unleashv1.UnleashList) {
	for i := range list.Items {
		crd := &list.Items[i]
		if !kubernetes.IsManagedByBifrost(crd) {
			continue
		}

		_, present, err := recordedIntent(crd)
		if err != nil {
			recordAdoptionEvent(adoptionEventRefusedInvalidIntent)
			r.logger.WithField("instance", crd.Name).WithError(err).
				Error("Refusing legacy adoption with malformed desired-state intent")
			continue
		}
		if present {
			continue
		}

		target, err := r.legacyAdoptionTarget(crd)
		if err != nil {
			recordAdoptionEvent(adoptionEventError)
			r.logger.WithField("instance", crd.Name).WithError(err).
				Error("Could not render canonical target for legacy adoption")
			return
		}
		log := r.logger.WithFields(logrus.Fields{
			"instance":      crd.Name,
			"spec_sections": driftingSpecSections(&crd.Spec, &target.Spec),
		})

		if r.dryRun {
			recordAdoptionEvent(adoptionEventDryRun)
			log.Info("Would canonicalize legacy CR and add a pending marker (dry-run)")
			return
		}
		if err := r.writeAdoptionTarget(ctx, crd, target); err != nil {
			recordAdoptionEvent(adoptionEventError)
			log.WithError(err).Error("Failed to canonicalize legacy CR")
			return
		}

		adoptionRemaining.Set(float64(adoptionRemainingCount(list) - 1))
		adoptionPending.Set(1)
		recordAdoptionEvent(adoptionEventAdmitted)
		log.Info("Canonicalized legacy CR; waiting for current-generation health")
		return
	}
}

func (r *UnleashReconciler) legacyAdoptionTarget(crd *unleashv1.Unleash) (*unleashv1.Unleash, error) {
	cfg, err := kubernetes.LoadConfigFromCRD(crd).Build()
	if err != nil {
		return nil, fmt.Errorf("derive Bifrost configuration from legacy CR: %w", err)
	}
	cfg.FederationNonce = crd.Spec.Federation.SecretNonce
	target := kubernetes.BuildUnleashCRD(r.config, cfg)
	return &target, nil
}

func (r *UnleashReconciler) writeAdoptionTarget(ctx context.Context, crd, target *unleashv1.Unleash) error {
	base := crd.DeepCopy()
	crd.Spec = target.Spec
	applyManagedMetadata(crd, target)
	if crd.Annotations == nil {
		crd.Annotations = make(map[string]string)
	}
	crd.Annotations[kubernetes.AnnotationAdoption] = adoptionPendingMarker

	if err := r.client.Patch(ctx, crd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch canonical legacy CR: %w", err)
	}
	return nil
}
