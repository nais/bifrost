package reconciler

import (
	"fmt"

	"github.com/nais/bifrost/pkg/config"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	client_go_scheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// NewManager builds a controller-runtime manager hosting the Unleash reconciler.
// The metrics server is disabled to avoid colliding with bifrost's HTTP server
// port; leader election is opt-in so the manager is safe to run in every replica
// once leases + RBAC are configured.
func NewManager(cfg *config.Config, logger *logrus.Logger) (manager.Manager, error) {
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

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("get kube config: %w", err)
	}

	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme:                  scheme,
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
