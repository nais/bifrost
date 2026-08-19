package reconciler

import (
	"strings"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/sirupsen/logrus"
)

// An empty namespace key is cache.AllNamespaces, so the manager must refuse to
// build rather than quietly watch the whole cluster. The check runs before any
// kubeconfig lookup, so this passes without a cluster.
func TestNewManager_RefusesEmptyInstanceNamespace(t *testing.T) {
	for _, ns := range []string{"", "   "} {
		cfg := testConfig()
		cfg.Unleash.InstanceNamespace = ns

		logger := logrus.New()
		logger.SetOutput(nopWriter{})

		if _, err := NewManager(cfg, logger); err != nil {
			if !strings.Contains(err.Error(), "BIFROST_UNLEASH_INSTANCE_NAMESPACE") {
				t.Errorf("NewManager(namespace=%q) error = %q, want it to name the variable", ns, err)
			}
			continue
		}
		t.Fatalf("NewManager(namespace=%q) returned a manager; want an error (an empty namespace key watches the whole cluster)", ns)
	}
}

func TestInstanceNamespace_ReturnsTheConfiguredName(t *testing.T) {
	ns, err := instanceNamespace(&config.Config{
		Unleash: config.UnleashConfig{InstanceNamespace: "bifrost-unleash"},
	})
	if err != nil {
		t.Fatalf("instanceNamespace: %v", err)
	}
	if ns != "bifrost-unleash" {
		t.Errorf("instanceNamespace = %q, want %q", ns, "bifrost-unleash")
	}
}

// Padding used to be trimmed here and nowhere else, which is worse than either
// accepting or refusing it: the adopter and the informer used the trimmed name
// while every other namespaced call — countInstances included — used the padded
// one, so adoption ran while the census that audits it failed forever.
func TestInstanceNamespace_RejectsWhitespacePadding(t *testing.T) {
	ns, err := instanceNamespace(&config.Config{
		Unleash: config.UnleashConfig{InstanceNamespace: " bifrost-unleash "},
	})
	if err == nil {
		t.Fatalf("instanceNamespace = %q, want an error; a padded namespace must be refused, not silently trimmed", ns)
	}
	if !strings.Contains(err.Error(), "BIFROST_UNLEASH_INSTANCE_NAMESPACE") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

// Adoption queues instances that carry no desired-state annotation. The
// reconciler refuses to converge those, but the manager refuses the combination
// outright: the safety of adoption must not rest on a single rule inside the
// reconcile loop.
func TestNewManager_RefusesAutoAdoptWithoutDryRun(t *testing.T) {
	cfg := testConfig()
	cfg.Reconciler.AutoAdopt = true
	cfg.Reconciler.DryRun = false

	logger := logrus.New()
	logger.SetOutput(nopWriter{})

	_, err := NewManager(cfg, logger)
	if err == nil {
		t.Fatal("NewManager returned a manager for autoAdopt with dry-run off; want an error")
	}
	for _, want := range []string{"BIFROST_RECONCILER_AUTO_ADOPT", "BIFROST_RECONCILER_DRY_RUN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("NewManager error = %q, want it to name %s", err, want)
		}
	}
}
