// Package reconciler contains bifrost's controller-runtime reconcile loop that
// continuously converges bifrost-managed Unleash instances to the configuration
// they should have. Unlike the request/response API, which applies config only
// on POST/PUT, the reconciler re-renders every managed instance on CR events and
// on a periodic resync, so global-config changes propagate and manual drift is
// corrected.
package reconciler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const defaultResyncInterval = 10 * time.Minute

// UnleashReconciler converges bifrost-managed Unleash CRs to their desired spec.
type UnleashReconciler struct {
	client client.Client
	config *config.Config
	logger *logrus.Logger
	resync time.Duration
	dryRun bool
}

// NewUnleashReconciler creates a reconciler. A non-positive resync falls back to
// the default interval. When dryRun is true the reconciler runs in observe mode:
// it computes and records what it would change but never writes.
func NewUnleashReconciler(c client.Client, cfg *config.Config, logger *logrus.Logger, resync time.Duration, dryRun bool) *UnleashReconciler {
	if resync <= 0 {
		resync = defaultResyncInterval
	}
	return &UnleashReconciler{client: c, config: cfg, logger: logger, resync: resync, dryRun: dryRun}
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
	// Counted, because this is not only the foreign-CR case: the watch predicate
	// filters on the label but a RequeueAfter does not, so an instance that
	// loses the label after having been reconciled comes back here exactly once
	// and then falls out of the reconciled set for good.
	if !kubernetes.IsManagedByBifrost(crd) {
		recordAction(actionSkipped, reasonMissingLabel)
		return ctrl.Result{}, nil
	}

	cfg, err := r.resolveIntent(crd)
	if err != nil {
		// Intent is unusable; do not thrash. Requeue on the slow resync so a
		// later fix (or a corrected annotation) is picked up. Counted, because
		// an instance that drops out of the managed set this way is otherwise
		// invisible: it stops reporting in_sync and the dashboard reads healthy.
		recordAction(actionIntentError, reasonNone)
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

	state := r.inSync(crd, &desired)
	if state.inSync {
		recordAction(actionInSync, reasonNone)
		return ctrl.Result{RequeueAfter: r.resync}, nil
	}
	log = state.annotate(log)

	// Observe mode (dark launch): record that a change is needed but do not write,
	// so the blast radius can be measured before the reconciler is allowed to act.
	if r.dryRun {
		recordAction(actionWouldChange, state.reason)
		log.Info("Instance differs from desired configuration (dry-run: no changes applied)")
		return ctrl.Result{RequeueAfter: r.resync}, nil
	}

	base := crd.DeepCopy()
	crd.Spec = desired.Spec
	// Carry forward the (possibly backfilled) managed-by label and desired-state
	// annotation from the render without dropping any foreign metadata.
	applyManagedMetadata(crd, &desired)

	// Optimistic lock: a plain MergeFrom omits resourceVersion, so a patch
	// computed from a stale cached read would land unconditionally and overwrite
	// a user update made in between — including the desired-state annotation
	// itself, after which every later reconcile would faithfully converge on the
	// old intent. A 409 instead triggers backoff and a re-read.
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.client.Patch(ctx, crd, patch); err != nil {
		recordAction(actionError, state.reason)
		log.WithError(err).Error("Failed to patch instance toward desired configuration")
		return ctrl.Result{}, err
	}

	recordAction(actionChanged, state.reason)
	log.Info("Reconciled instance to desired configuration")
	return ctrl.Result{RequeueAfter: r.resync}, nil
}

// resolveIntent returns the per-instance config, preferring the authoritative
// desired-state annotation and falling back to reverse-engineering the spec for
// instances created before the annotation existed.
func (r *UnleashReconciler) resolveIntent(crd *unleashv1.Unleash) (*unleash.Config, error) {
	if raw := crd.GetAnnotations()[kubernetes.AnnotationDesiredState]; raw != "" {
		cfg, err := kubernetes.UnmarshalIntent(raw)
		if err != nil {
			return nil, err
		}

		// The annotation is authoritative, so whatever it says gets rendered
		// onto a live instance. Malformed JSON already fails above, but valid
		// JSON with wrong contents did not: "{}" unmarshals to an all-zero
		// config and would render empty ingress hosts and a zero database pool.
		// Hold it to the same rules the builder enforces.
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("desired-state annotation is not a valid config: %w", err)
		}

		// And confirm it belongs to this instance. A copied or mis-templated
		// annotation would otherwise converge one instance onto another's
		// configuration, including its ingress hosts and database secret.
		if cfg.Name != crd.GetName() {
			return nil, fmt.Errorf("desired-state annotation names %q but is on instance %q", cfg.Name, crd.GetName())
		}

		return cfg, nil
	}
	return kubernetes.LoadConfigFromCRD(crd).Build()
}

// syncState is the outcome of comparing a live instance to its render: whether
// it matches, and if not, why. reason is one of a closed set and is safe as a
// metric label; sections is open-ended and belongs only in the log.
type syncState struct {
	inSync   bool
	reason   string
	sections []string
}

// annotate attaches the drift cause to the log entry, so a would_change firing
// can be attributed to a cause — and, for a spec mismatch, to the spec sections
// involved — without re-rendering the instance by hand.
func (s syncState) annotate(log *logrus.Entry) *logrus.Entry {
	log = log.WithField("reason", s.reason)
	if len(s.sections) > 0 {
		log = log.WithField("spec_sections", strings.Join(s.sections, ","))
	}
	return log
}

// inSync reports whether the live spec and managed metadata already match the
// desired render, so a no-op reconcile issues no patch. The checks are ordered
// most to least significant: a spec mismatch is the cause an operator has to act
// on, the metadata ones are backfill that dry-run can never clear on its own.
func (r *UnleashReconciler) inSync(live, desired *unleashv1.Unleash) syncState {
	if !equality.Semantic.DeepEqual(live.Spec, desired.Spec) {
		return syncState{reason: reasonSpecMismatch, sections: driftingSpecSections(&live.Spec, &desired.Spec)}
	}
	if live.GetLabels()[kubernetes.LabelManagedBy] != kubernetes.ManagedByBifrost {
		return syncState{reason: reasonMissingLabel}
	}
	if live.GetAnnotations()[kubernetes.AnnotationDesiredState] != desired.GetAnnotations()[kubernetes.AnnotationDesiredState] {
		return syncState{reason: reasonIntentMismatch}
	}
	return syncState{inSync: true, reason: reasonNone}
}

// driftingSpecSections names the top-level UnleashSpec fields that differ. It
// walks the struct reflectively rather than listing the fields so a field added
// upstream is reported instead of silently omitted, and it reports names only:
// the values can hold the federation secret nonce and rendered env vars, which
// have no business in a log line.
func driftingSpecSections(live, desired *unleashv1.UnleashSpec) []string {
	liveFields := reflect.ValueOf(*live)
	desiredFields := reflect.ValueOf(*desired)

	var sections []string
	for i := 0; i < liveFields.NumField(); i++ {
		if !equality.Semantic.DeepEqual(liveFields.Field(i).Interface(), desiredFields.Field(i).Interface()) {
			sections = append(sections, liveFields.Type().Field(i).Name)
		}
	}
	return sections
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
	// The fleet-wide work — adoption and the census behind the gauges — runs on
	// a timer rather than per Reconcile. The List is served from the manager's
	// cache, but running it per instance per resync makes the work quadratic in
	// fleet size for numbers that only have to be accurate enough to compare
	// against the action counters.
	if err := mgr.Add(manager.RunnableFunc(r.runFleetSweep)); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&unleashv1.Unleash{}, builder.WithPredicates(managed)).
		Named("bifrost-unleash").
		Complete(r)
}

// runFleetSweep adopts unlabelled instances and keeps the fleet gauges current
// until the manager stops. Added as a plain Runnable, so with leader election on
// only the leader sweeps — matching the action counters, which only advance
// there.
//
// Adoption and the census share one runnable, in that order, so every census
// reports the fleet as the sweep just left it. That ordering is what makes
// unmanaged_instances readable during a migration: once adoption has run,
// whatever is still unmanaged is opted out or failed to stamp — not merely
// not-yet-visited — and the step in the gauges lines up with the adoptions
// counted in the same pass. On separate timers a census could land mid-sweep
// and publish, and timestamp, a split that says neither.
//
// The per-instance side needs no such ordering: stamping a label is idempotent
// and immediately queues that one instance through the watch, so a partial
// sweep just means fewer instances are visible yet, never a wrong number for
// the ones that are.
func (r *UnleashReconciler) runFleetSweep(ctx context.Context) error {
	ticker := time.NewTicker(r.resync)
	defer ticker.Stop()

	for {
		if r.config.Reconciler.AutoAdopt {
			r.adoptFleet(ctx)
		}
		r.countInstances(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// countInstances sets the fleet gauges from a List of the whole namespace and
// partitions the result itself. Listing with a managed-by selector instead —
// which is what this did — cannot observe an instance losing the label, because
// that removes it from the count and from the reconciled set in the same step:
// the gauge steps down by one and is indistinguishable from a deletion.
//
// A failure is logged and the previous values left in place: a zero would read
// as "the fleet disappeared". The timestamp is deliberately not advanced, so a
// census that keeps failing is visible as a stale one rather than as a plausible
// steady state.
func (r *UnleashReconciler) countInstances(ctx context.Context) {
	list := &unleashv1.UnleashList{}
	if err := r.client.List(ctx, list, client.InNamespace(r.config.Unleash.InstanceNamespace)); err != nil {
		r.logger.WithError(err).Warn("Failed to count Unleash instances")
		return
	}

	var managed float64
	for i := range list.Items {
		if kubernetes.IsManagedByBifrost(&list.Items[i]) {
			managed++
		}
	}
	managedInstances.Set(managed)
	unmanagedInstances.Set(float64(len(list.Items)) - managed)
	instancesUpdatedTimestamp.SetToCurrentTime()
}
