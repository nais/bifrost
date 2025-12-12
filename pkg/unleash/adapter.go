package unleash

import (
	"context"
	"errors"

	"github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	domainUnleash "github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/cloudsql"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

// ServiceAdapter wraps the new application service to provide the old IUnleashService interface
// This ensures backward compatibility with existing handlers while using the new architecture
type ServiceAdapter struct {
	service *unleash.Service
	config  *config.Config
}

// NewServiceAdapter creates an adapter that bridges old and new service interfaces
func NewServiceAdapter(sqlDatabasesClient ISQLDatabasesService, sqlUsersClient ISQLUsersService, kubeClient ctrl.Client, config *config.Config, logger *logrus.Logger) *ServiceAdapter {
	// Wrap SQL clients
	dbManager := cloudsql.NewManager(sqlDatabasesClient, sqlUsersClient, kubeClient, config, logger)

	// Create repository
	repo := kubernetes.NewUnleashRepository(kubeClient, config, logger)

	// Create new service
	service := unleash.NewService(repo, dbManager, config, logger)

	return &ServiceAdapter{
		service: service,
		config:  config,
	}
}

// List implements IUnleashService.List by converting domain instances to old UnleashInstance format
// For v0 API, this excludes instances with release channels (treated as if they don't exist)
func (a *ServiceAdapter) List(ctx context.Context) ([]*UnleashInstance, error) {
	// Exclude channel instances for backward compatibility with v0
	instances, err := a.service.List(ctx, true)
	if err != nil {
		return nil, err
	}

	oldInstances := make([]*UnleashInstance, len(instances))
	for i, instance := range instances {
		oldInstances[i] = domainInstanceToOld(instance)
	}

	return oldInstances, nil
}

// Get implements IUnleashService.Get by converting domain instance to old UnleashInstance format
// Returns error if instance uses release channel (v0 should not access these)
func (a *ServiceAdapter) Get(ctx context.Context, name string) (*UnleashInstance, error) {
	instance, err := a.service.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	// V0 API should not access channel instances
	if instance.HasReleaseChannel() {
		return nil, errors.New("instance not found") // Treat as not found for v0
	}

	return domainInstanceToOld(instance), nil
}

// Create implements IUnleashService.Create by converting UnleashConfig to domain Config
func (a *ServiceAdapter) Create(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	config, err := oldConfigToDomain(uc)
	if err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to build configuration"}
	}

	crdInstance, err := a.service.Create(ctx, config)
	if err != nil {
		return nil, err
	}

	return crdInstance, nil
}

// Update implements IUnleashService.Update by converting UnleashConfig to domain Config
func (a *ServiceAdapter) Update(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	// First check if instance exists and doesn't have a channel (v0 protection)
	existing, err := a.service.Get(ctx, uc.Name)
	if err != nil {
		return nil, err
	}

	if existing.HasReleaseChannel() {
		return nil, &UnleashError{
			Err:    errors.New("cannot modify instance with release channel"),
			Reason: "this instance is managed by a release channel and cannot be modified through v0 API",
		}
	}

	config, err := oldConfigToDomain(uc)
	if err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to build configuration"}
	}

	crdInstance, err := a.service.Update(ctx, config)
	if err != nil {
		return nil, err
	}

	return crdInstance, nil
}

// Delete implements IUnleashService.Delete
func (a *ServiceAdapter) Delete(ctx context.Context, name string) error {
	// First check if instance exists and doesn't have a channel (v0 protection)
	existing, err := a.service.Get(ctx, name)
	if err != nil {
		return err
	}

	if existing.HasReleaseChannel() {
		return &UnleashError{
			Err:    errors.New("cannot delete instance with release channel"),
			Reason: "this instance is managed by a release channel and cannot be deleted through v0 API",
		}
	}

	return a.service.Delete(ctx, name)
}

// oldConfigToDomain converts old UnleashConfig to new domain Config
func oldConfigToDomain(uc *UnleashConfig) (*domainUnleash.Config, error) {
	builder := domainUnleash.NewConfigBuilder().
		WithName(uc.Name).
		WithCustomVersion(uc.CustomVersion).
		WithLogLevel(uc.LogLevel).
		WithDatabasePool(uc.DatabasePoolMax, uc.DatabasePoolIdleTimeoutMs)

	if uc.EnableFederation {
		builder.WithFederation(uc.FederationNonce, uc.AllowedTeams, uc.AllowedNamespaces, uc.AllowedClusters)
	}

	return builder.Build()
}

// domainInstanceToOld converts new domain Instance to old UnleashInstance
// Note: The ServerInstance field needs to be populated by calling the Kubernetes API separately
func domainInstanceToOld(instance *domainUnleash.Instance) *UnleashInstance {
	// Build a basic Unleash CRD object from the domain instance data
	// This is a read-only representation for API responses
	crd := &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:              instance.Name,
			Namespace:         instance.Namespace,
			CreationTimestamp: metav1.NewTime(instance.CreatedAt),
		},
		Status: unleashv1.UnleashStatus{
			Version: instance.Version,
		},
		Spec: unleashv1.UnleashSpec{
			ApiIngress: unleashv1.UnleashIngressConfig{
				Host: extractHost(instance.APIUrl),
			},
			WebIngress: unleashv1.UnleashIngressConfig{
				Host: extractHost(instance.WebUrl),
			},
		},
	}

	// Set custom version if present
	if instance.CustomVersion != "" {
		crd.Spec.CustomImage = instance.CustomVersion
	}

	return &UnleashInstance{
		Name:                instance.Name,
		KubernetesNamespace: instance.Namespace,
		CreatedAt:           metav1.NewTime(instance.CreatedAt),
		ServerInstance:      crd,
	}
}

// extractHost extracts the hostname from a URL
func extractHost(urlStr string) string {
	// URLs are in format "https://hostname/path/"
	// Extract just the hostname part
	if len(urlStr) > 8 && urlStr[:8] == "https://" {
		urlStr = urlStr[8:]
	}
	if len(urlStr) > 7 && urlStr[:7] == "http://" {
		urlStr = urlStr[7:]
	}
	// Find the first slash and take everything before it
	for i, c := range urlStr {
		if c == '/' {
			return urlStr[:i]
		}
	}
	return urlStr
}
