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

func TestInstanceNamespace_TrimsAndReturns(t *testing.T) {
	ns, err := instanceNamespace(&config.Config{
		Unleash: config.UnleashConfig{InstanceNamespace: " bifrost-unleash "},
	})
	if err != nil {
		t.Fatalf("instanceNamespace: %v", err)
	}
	if ns != "bifrost-unleash" {
		t.Errorf("instanceNamespace = %q, want %q", ns, "bifrost-unleash")
	}
}
