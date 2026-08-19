package reconciler

import (
	"context"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// adoptFleet stamps the managed-by label on Unleash instances in bifrost's own
// namespace that do not carry it yet.
//
// It exists because the reconciler cannot do this itself: both the watch
// predicate and the in-loop check are gated on that same label, so an unlabelled
// instance never enters the work queue and the fleet bifrost created is invisible
// to it. Without adoption the queue stays empty and every counter reads zero,
// which is exactly what a healthy fleet also looks like.
//
// Adoption is additive and reversible: it only ever adds the label, never
// deletes, never touches spec, and is undone by removing the label again.
func (r *UnleashReconciler) adoptFleet(ctx context.Context) {
	// Reuses the manager's guard rather than repeating it. An empty namespace is
	// metav1.NamespaceAll, and adoption is the one loop here that writes to
	// every object it lists — cluster-wide, that would stamp bifrost's label on
	// every tenant Unleash CR unleasherator owns.
	ns, err := instanceNamespace(r.config)
	if err != nil {
		r.logger.WithError(err).Error("Refusing to adopt Unleash instances")
		return
	}

	list := &unleashv1.UnleashList{}
	if err := r.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		r.logger.WithError(err).Warn("Failed to list Unleash instances for adoption")
		return
	}

	for i := range list.Items {
		crd := &list.Items[i]
		if !adoptable(crd) {
			continue
		}

		log := r.logger.WithField("instance", crd.GetName())
		if err := r.adopt(ctx, crd); err != nil {
			adoptionsTotal.WithLabelValues(adoptionError).Inc()
			log.WithError(err).Warn("Failed to adopt Unleash instance")
			continue
		}

		adoptionsTotal.WithLabelValues(adoptionAdopted).Inc()
		log.Info("Adopted Unleash instance into the bifrost-managed fleet")
	}
}

// adoptable reports whether an instance may be stamped.
func adoptable(crd *unleashv1.Unleash) bool {
	labels := crd.GetLabels()

	// Never take an object another controller has claimed. managed-by set to
	// anything but bifrost means it belongs to that controller, and re-pointing
	// it here would hand one object to two reconcilers. Already-bifrost is a
	// no-op, so it falls out of the same check.
	if _, claimed := labels[kubernetes.LabelManagedBy]; claimed {
		return false
	}

	return labels[kubernetes.LabelAdopt] != kubernetes.AdoptOptOut
}

// adopt writes the managed-by label and nothing else.
//
// Specifically not the desired-state annotation, which is the whole point of
// adopting by label. The label makes an instance *visible*: queued, counted,
// drift reported. The annotation makes an intent *authoritative* — resolveIntent
// prefers it and the reconciler converges the instance to it. The only intent
// available here comes from LoadConfigFromCRD, which is a lossy read-back of the
// rendered spec, so stamping it would promote what that read-back dropped into
// the declared truth, permanently and unrecoverably. Left absent, resolveIntent
// falls back to exactly the same read-back, so drift is measured identically
// without recording an intent we do not trust.
func (r *UnleashReconciler) adopt(ctx context.Context, crd *unleashv1.Unleash) error {
	base := crd.DeepCopy()

	if crd.Labels == nil {
		crd.Labels = map[string]string{}
	}
	crd.Labels[kubernetes.LabelManagedBy] = kubernetes.ManagedByBifrost

	// Optimistic lock so a stale cached read cannot re-add a label somebody just
	// removed to hand the instance elsewhere. A conflict is not worth retrying
	// inline: the next sweep re-lists and picks it up.
	return r.client.Patch(ctx, crd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
