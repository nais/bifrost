package reconciler

import (
	"context"
	"strings"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// adoptFleet stamps the managed-by label on at most one Unleash instance per
// sweep, in bifrost's own namespace, on instances that do not carry it yet.
//
// It exists because the reconciler cannot do this itself: both the watch
// predicate and the in-loop check are gated on that same label, so an unlabelled
// instance never enters the work queue and the fleet bifrost created is invisible
// to it. Without adoption the queue stays empty and every counter reads zero,
// which is exactly what a healthy fleet also looks like.
//
// Adoption is additive and reversible: it only ever adds the label, never
// deletes, never touches spec, and is undone by removing the label again.
//
// One per sweep, because the label is not inert outside bifrost. Unleasherator
// filters its own watch on predicate.Or(GenerationChangedPredicate,
// LabelChangedPredicate) (nais/unleasherator, internal/controller/unleash_controller.go,
// SetupWithManager), so every stamp wakes a reconcile there, at
// MaxConcurrentReconciles: 4. Most of those are no-ops — but any instance whose
// workload has drifted from what unleasherator renders gets converged right
// then, and each one ends in a live connectivity check against the Unleash
// instance. Stamping the 63 unlabelled production instances in one pass turns a
// metadata migration into a fleet-wide burst of deferred convergence. The
// sweep's own ticker (resync, 10m by default) is the pacing mechanism: no timer
// is added and nothing sleeps here, because a runnable that sleeps holds its
// slot and ignores cancellation.
func (r *UnleashReconciler) adoptFleet(ctx context.Context) {
	// A halt is latched and deliberately not self-clearing: the point of
	// stopping is that a human looks at the instance named in the log before
	// any more of the fleet is touched. Cleared only by a process restart or by
	// turning autoAdopt off and on again.
	if r.adoptionHalted {
		r.logger.WithField("instance", r.adoptionWatched).
			Warn("Fleet adoption is halted after a stamped instance stopped being healthy; no instances will be adopted until bifrost restarts")
		return
	}

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

	// The previous tick's stamp is judged before another one is made, so a
	// migration that is going wrong stops after one instance rather than after
	// the fleet.
	if !r.previousStampIsStillHealthy(list) {
		return
	}

	for i := range list.Items {
		crd := &list.Items[i]
		log := r.logger.WithField("instance", crd.GetName())

		if !r.adoptable(crd, log) {
			continue
		}

		// Health-gated, on the instance's own status: an instance that is
		// already degraded, or that has never reported a successful reconcile,
		// is the worst possible one to hand an extra unleasherator reconcile
		// and a connectivity check to.
		if reason, ok := adoptionCandidateIsHealthy(crd); !ok {
			adoptionsTotal.WithLabelValues(adoptionUnhealthy).Inc()
			log.WithField("status", reason).Info("Skipping adoption of Unleash instance that is not healthy; it stays a candidate for a later sweep")
			continue
		}

		if err := r.adopt(ctx, crd); err != nil {
			// Not a halt, and not the sweep's one stamp: a failed patch changed
			// nothing, so the next candidate in this same sweep is still the
			// first instance being adopted. Stopping here instead would let one
			// permanently forbidden instance block the whole migration.
			adoptionsTotal.WithLabelValues(adoptionError).Inc()
			log.WithError(err).Warn("Failed to adopt Unleash instance")
			continue
		}

		adoptionsTotal.WithLabelValues(adoptionAdopted).Inc()
		r.adoptionWatched = crd.GetName()
		log.Info("Adopted Unleash instance into the bifrost-managed fleet; the next candidate waits for the next sweep")

		// One per sweep. Everything still unlabelled is picked up on the next
		// tick, in list order, so the fleet migrates at one instance per resync
		// interval and every stamp is judged before the next is made.
		return
	}
}

// previousStampIsStillHealthy re-reads the instance the previous sweep stamped
// and reports whether adoption may continue. A false return means the sweep has
// halted; this is the only thing that sets that latch.
//
// This is the check the pacing exists for. Stamping wakes unleasherator, which
// may then converge a workload that has drifted since it was last written, so
// the interesting failure is not the stamp but what happens to the instance
// afterwards — and that only becomes visible a reconcile later.
//
// What this can and cannot prove, because a future reader will assume the
// stronger property: a label edit does not bump metadata.generation, so
// observedGeneration never moves for our write, and a condition that was already
// True and stays True keeps its old lastTransitionTime. Nothing in the status
// says "unleasherator has processed *this* stamp". The gate is therefore health,
// not acknowledgement: it asserts that the instance we touched is not visibly
// worse than it was, and it cannot tell an instance unleasherator has
// re-reconciled cleanly from one it has not looked at yet. That is a weaker
// guarantee than it appears, and it is exactly why the pacing is one instance
// per resync interval rather than one per second — the interval is what gives
// the unobservable reconcile time to happen, and to show up in the status if it
// went badly.
func (r *UnleashReconciler) previousStampIsStillHealthy(list *unleashv1.UnleashList) bool {
	if r.adoptionWatched == "" {
		return true
	}

	var watched *unleashv1.Unleash
	for i := range list.Items {
		if list.Items[i].GetName() == r.adoptionWatched {
			watched = &list.Items[i]
			break
		}
	}
	if watched == nil {
		// Deleted, or moved out of the namespace, between sweeps. There is no
		// status left to judge, and a missing object is not evidence of harm —
		// a tenant deleting an instance looks exactly like this — so adoption
		// continues, but says so rather than forgetting silently.
		r.logger.WithField("instance", r.adoptionWatched).
			Info("Previously adopted Unleash instance is gone; continuing adoption without verifying it")
		r.adoptionWatched = ""
		return true
	}

	if reason, regressed := adoptionWatchHasRegressed(watched); regressed {
		r.adoptionHalted = true
		adoptionsTotal.WithLabelValues(adoptionHalted).Inc()
		adoptionHalt.Set(1)
		// Error rather than warn, and the instance is named: this is the one
		// outcome that wants a human before anything else is touched, and the
		// gauge alone cannot say which instance to look at.
		r.logger.WithField("instance", watched.GetName()).WithField("status", reason).
			Error("Unleash instance regressed after bifrost adopted it; halting fleet adoption until someone has looked at it")
		return false
	}

	// Kept, not cleared: if this sweep finds nothing to adopt, the same instance
	// is judged again next tick, which is strictly more watching rather than
	// less. It is replaced the moment another instance is stamped.
	return true
}

// adoptionCandidateIsHealthy reports whether an instance's status makes it safe
// to stamp, and names the reason when it does not.
//
// Deliberately conservative, and deliberately not the mirror image of
// adoptionWatchHasRegressed: an instance must positively say Reconciled=True, so
// an absent or Unknown status disqualifies it. Unleasherator writes these
// conditions on every pass, so a candidate carrying none has never reconciled
// successfully — the last instance that should be handed an extra reconcile.
// Skipping is not a failure: the instance stays a candidate and is retried on
// every later sweep, which is why it is counted rather than logged only.
func adoptionCandidateIsHealthy(crd *unleashv1.Unleash) (string, bool) {
	conditions := crd.Status.Conditions

	if meta.IsStatusConditionTrue(conditions, unleashv1.UnleashStatusConditionTypeDegraded) {
		return "Degraded=True", false
	}
	if !meta.IsStatusConditionTrue(conditions, unleashv1.UnleashStatusConditionTypeReconciled) {
		return "Reconciled is not True", false
	}
	return "", true
}

// adoptionWatchHasRegressed reports whether the instance stamped last sweep has
// visibly got worse, and names how.
//
// The bar is higher than adoptionCandidateIsHealthy's on purpose. That one
// answers "is this a safe instance to touch", where an unknown answer means
// wait. This one answers "did touching it break something", where the
// consequence is halting the entire migration, so only an unambiguous statement
// counts: Degraded=True, or Reconciled explicitly flipped to False. A condition
// that has gone Unknown is not a regression — unleasherator writes exactly that
// while finalizing an instance being deleted — and treating it as one would stop
// adoption every time a tenant deletes an instance.
func adoptionWatchHasRegressed(crd *unleashv1.Unleash) (string, bool) {
	conditions := crd.Status.Conditions

	if meta.IsStatusConditionTrue(conditions, unleashv1.UnleashStatusConditionTypeDegraded) {
		return "Degraded=True", true
	}
	if meta.IsStatusConditionFalse(conditions, unleashv1.UnleashStatusConditionTypeReconciled) {
		return "Reconciled=False", true
	}
	return "", false
}

// adoptable reports whether an instance may be stamped, and says so out loud in
// the two cases where the answer is not the one the labels look like they mean.
func (r *UnleashReconciler) adoptable(crd *unleashv1.Unleash, log *logrus.Entry) bool {
	labels := crd.GetLabels()

	// Never take an object another controller has claimed. managed-by set to
	// anything but bifrost means it belongs to that controller, and re-pointing
	// it here would hand one object to two reconcilers. Already-bifrost is a
	// no-op, so it falls out of the same check.
	if owner, claimed := labels[kubernetes.LabelManagedBy]; claimed {
		// Present-but-empty names no controller, so it is a claim nobody can
		// act on: never adopted, never reconciled, counted as unmanaged for
		// good. Every other exclusion here is either visible in the label or
		// deliberate; this one reads as a typo and behaves as a permanent
		// exemption, so it is the one that has to be said out loud.
		if strings.TrimSpace(owner) == "" {
			log.Warnf("Instance has an empty %s label, which excludes it from adoption and from the reconciler forever; remove the label to make it adoptable", kubernetes.LabelManagedBy)
		}
		return false
	}

	// Fail toward being visible: only the exact opt-out value exempts, so an
	// unrecognised one is adopted. That is the safe direction — adoption adds a
	// label and is undone by removing it — but it means a mistyped "False"
	// silently does the opposite of what its author intended.
	if value, ok := labels[kubernetes.LabelAdopt]; ok && value != kubernetes.AdoptOptOut && value != kubernetes.AdoptOptIn {
		log.Warnf("Instance has %s=%q, which is not a recognised value; only the exact value %q opts out of adoption", kubernetes.LabelAdopt, value, kubernetes.AdoptOptOut)
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
