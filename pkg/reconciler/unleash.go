// Package reconciler contains bifrost's controller-runtime reconcile loop that
// continuously converges bifrost-managed Unleash instances to the configuration
// they should have. Unlike the request/response API, which applies config only
// on POST/PUT, the reconciler re-renders every managed instance on CR events and
// on a periodic resync, so global-config changes propagate and manual drift is
// corrected.
package reconciler

import (
	"context"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const defaultResyncInterval = 10 * time.Minute

// UnleashReconciler converges bifrost-managed Unleash CRs to their desired spec.
type UnleashReconciler struct {
	client client.Client
	config *config.Config
	logger *logrus.Logger
	resync time.Duration
}

// NewUnleashReconciler creates a reconciler. A non-positive resync falls back to
// the default interval.
func NewUnleashReconciler(c client.Client, cfg *config.Config, logger *logrus.Logger, resync time.Duration) *UnleashReconciler {
	if resync <= 0 {
		resync = defaultResyncInterval
	}
	return &UnleashReconciler{client: c, config: cfg, logger: logger, resync: resync}
}

// Reconcile renders the desired spec from the instance's intent and patches the
// live CR toward it, preserving metadata, finalizers, ownerReferences, and the
// status written by unleasherator.
func (r *UnleashReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.logger.WithField("instance", req.Name)

	crd := &unleashv1.Unleash{}
	if err := r.client.Get(ctx, req.NamespacedName, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only touch instances bifrost owns; never a hand-authored or foreign CR.
	if !kubernetes.IsManagedByBifrost(crd) {
		return ctrl.Result{}, nil
	}

	cfg, err := r.resolveIntent(crd)
	if err != nil {
		// Intent is unusable; do not thrash. Requeue on the slow resync so a
		// later fix (or a corrected annotation) is picked up.
		log.WithError(err).Error("Failed to resolve desired-state intent; skipping reconcile")
		return ctrl.Result{RequeueAfter: r.resync}, nil
	}

	// Preserve the existing federation nonce so rendering is deterministic;
	// otherwise BuildUnleashCRD would mint a fresh random nonce every reconcile
	// and the spec would never converge.
	if crd.Spec.Federation.SecretNonce != "" {
		cfg.FederationNonce = crd.Spec.Federation.SecretNonce
	}

	desired := kubernetes.BuildUnleashCRD(r.config, cfg)

	if r.inSync(crd, &desired) {
		return ctrl.Result{RequeueAfter: r.resync}, nil
	}

	base := crd.DeepCopy()
	crd.Spec = desired.Spec
	// Carry forward the (possibly backfilled) managed-by label and desired-state
	// annotation from the render without dropping any foreign metadata.
	applyManagedMetadata(crd, &desired)

	if err := r.client.Patch(ctx, crd, client.MergeFrom(base)); err != nil {
		log.WithError(err).Error("Failed to patch instance toward desired configuration")
		return ctrl.Result{}, err
	}

	log.Info("Reconciled instance to desired configuration")
	return ctrl.Result{RequeueAfter: r.resync}, nil
}

// resolveIntent returns the per-instance config, preferring the authoritative
// desired-state annotation and falling back to reverse-engineering the spec for
// instances created before the annotation existed.
func (r *UnleashReconciler) resolveIntent(crd *unleashv1.Unleash) (*unleash.Config, error) {
	if raw := crd.GetAnnotations()[kubernetes.AnnotationDesiredState]; raw != "" {
		return kubernetes.UnmarshalIntent(raw)
	}
	return kubernetes.LoadConfigFromCRD(crd).Build()
}

// inSync reports whether the live spec and managed metadata already match the
// desired render, so a no-op reconcile issues no patch.
func (r *UnleashReconciler) inSync(live, desired *unleashv1.Unleash) bool {
	if !equality.Semantic.DeepEqual(live.Spec, desired.Spec) {
		return false
	}
	if live.GetLabels()[kubernetes.LabelManagedBy] != kubernetes.ManagedByBifrost {
		return false
	}
	return live.GetAnnotations()[kubernetes.AnnotationDesiredState] == desired.GetAnnotations()[kubernetes.AnnotationDesiredState]
}

// applyManagedMetadata copies bifrost's managed-by label and desired-state
// annotation from the render onto the live object, leaving all other metadata
// intact.
func applyManagedMetadata(live, desired *unleashv1.Unleash) {
	if live.Labels == nil {
		live.Labels = map[string]string{}
	}
	live.Labels[kubernetes.LabelManagedBy] = kubernetes.ManagedByBifrost

	if desired.GetAnnotations()[kubernetes.AnnotationDesiredState] != "" {
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[kubernetes.AnnotationDesiredState] = desired.Annotations[kubernetes.AnnotationDesiredState]
	}
}

// SetupWithManager registers the reconciler, filtered to bifrost-managed
// instances so foreign Unleash CRs never enter the work queue.
func (r *UnleashReconciler) SetupWithManager(mgr ctrl.Manager) error {
	managed, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{kubernetes.LabelManagedBy: kubernetes.ManagedByBifrost},
	})
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&unleashv1.Unleash{}, builder.WithPredicates(managed)).
		Named("bifrost-unleash").
		Complete(r)
}
