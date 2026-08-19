package reconciler

import (
	"fmt"
	"strings"

	"github.com/nais/bifrost/pkg/config"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	client_go_scheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// NewManager builds a controller-runtime manager hosting the Unleash reconciler.
// The metrics server is disabled to avoid colliding with bifrost's HTTP server
// port — controller-runtime's registry is instead gathered by bifrost's own
// /metrics route; leader election is opt-in so the manager is safe to run in
// every replica once leases + RBAC are configured.
func NewManager(cfg *config.Config, logger *logrus.Logger) (manager.Manager, error) {
	ns, err := instanceNamespace(cfg)
	if err != nil {
		return nil, err
	}
	if err := checkAdoptionIsObserveOnly(cfg); err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	if err := client_go_scheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add client-go scheme: %w", err)
	}
	if err := unleashv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add unleash scheme: %w", err)
	}
	if err := fqdnV1alpha3.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add fqdn scheme: %w", err)
	}

	// controller-runtime discards its own logr output until a logger is set, so
	// wire it to bifrost's logger before any of it is constructed.
	ctrl.SetLogger(NewLogger(logger))

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("get kube config: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: scheme,
		// Scope the informer to the namespace bifrost is granted. Without this
		// the cache does a cluster-scoped list/watch, which the chart's
		// namespaced Role forbids, so the cache never syncs and the manager
		// fails to start.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				ns: {},
			},
		},
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          cfg.Reconciler.LeaderElection,
		LeaderElectionID:        "bifrost-reconciler.nais.io",
		LeaderElectionNamespace: cfg.Reconciler.LeaderElectionNamespace,
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	r := NewUnleashReconciler(mgr.GetClient(), cfg, logger, cfg.Reconciler.ResyncInterval, cfg.Reconciler.DryRun)
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up reconciler: %w", err)
	}

	return mgr, nil
}

// instanceNamespace returns the namespace bifrost is scoped to, refusing an
// empty one.
//
// Config.Validate already rejects this at startup, and this is not a duplicate
// of that check but the place its consequence lands: cache.AllNamespaces is
// metav1.NamespaceAll, so "" as the DefaultNamespaces key does not scope the
// informer down, it scopes it up to the whole cluster. A single check guarding
// a silent, scope-widening bug is one refactor away from being no check at all,
// so the manager refuses independently — and the fleet adopter reuses this
// rather than growing a third copy.
func instanceNamespace(cfg *config.Config) (string, error) {
	ns := cfg.Unleash.InstanceNamespace
	if strings.TrimSpace(ns) == "" {
		return "", fmt.Errorf("BIFROST_UNLEASH_INSTANCE_NAMESPACE is empty: an empty namespace means all namespaces, not none")
	}
	// Rejected rather than trimmed. Trimming made this the only place the
	// padding disappeared: everything else — every namespaced List/Get/Delete
	// and the census — used the raw value, so " bifrost-unleash " passed
	// validation, adopted instances under the trimmed name, and left the census
	// failing forever with "unknown namespace for the cache". One name, or no
	// start.
	if ns != strings.TrimSpace(ns) {
		return "", fmt.Errorf("BIFROST_UNLEASH_INSTANCE_NAMESPACE has leading or trailing whitespace (%q): a namespace name cannot contain it, and trimming it here would leave the rest of bifrost using the padded value", ns)
	}
	return ns, nil
}

// checkAdoptionIsObserveOnly refuses the one settings combination in which
// adoption stops being additive.
//
// Adoption's job is to put instances created before the desired-state
// annotation existed into the reconcile queue, and none of them carries an
// intent. The reconciler observes those instances and never writes them (see
// Reconcile), so the combination is survivable on its own — but that safety
// rests on a rule one edit away from being relaxed, and the blast radius on the
// other side of it is the entire fleet rewritten from a lossy read-back of its
// own spec. Config.Validate rejects this at startup; this is the second,
// independent refusal, at the point where the writing reconciler is actually
// constructed.
func checkAdoptionIsObserveOnly(cfg *config.Config) error {
	if cfg.Reconciler.AutoAdopt && !cfg.Reconciler.DryRun {
		return fmt.Errorf("BIFROST_RECONCILER_AUTO_ADOPT=true requires BIFROST_RECONCILER_DRY_RUN=true: adoption queues instances that have no desired-state annotation, and converging one means rendering it from a lossy read-back of its own spec")
	}
	return nil
}
