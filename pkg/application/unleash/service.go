package unleash

import (
	"context"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
)

// IService defines the interface for unleash instance management operations
type IService interface {
	List(ctx context.Context, excludeChannelInstances bool) ([]*unleash.Instance, error)
	ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error)
	Get(ctx context.Context, name string) (*unleash.Instance, error)
	GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error)
	Create(ctx context.Context, config *unleash.Config) (*unleashv1.Unleash, error)
	Update(ctx context.Context, config *unleash.Config) (*unleashv1.Unleash, error)
	Delete(ctx context.Context, name string) error
}

// DatabaseManager defines the interface for database operations
type DatabaseManager interface {
	CreateDatabase(ctx context.Context, name string) error
	CreateDatabaseUser(ctx context.Context, name string) (string, error)
	CreateSecret(ctx context.Context, name string, password string) error
	DeleteDatabase(ctx context.Context, name string) error
	DeleteDatabaseUser(ctx context.Context, name string) error
	DeleteSecret(ctx context.Context, name string) error
}

// Service orchestrates unleash instance management operations
type Service struct {
	repository unleash.Repository
	dbManager  DatabaseManager
	config     *config.Config
	logger     *logrus.Logger
}

// NewService creates a new unleash application service
func NewService(repository unleash.Repository, dbManager DatabaseManager, config *config.Config, logger *logrus.Logger) *Service {
	return &Service{
		repository: repository,
		dbManager:  dbManager,
		config:     config,
		logger:     logger,
	}
}

// List returns all unleash instances, optionally excluding those with release channels
func (s *Service) List(ctx context.Context, excludeChannelInstances bool) ([]*unleash.Instance, error) {
	return s.repository.List(ctx, excludeChannelInstances)
}

// ListCRDs returns all unleash CRDs, optionally excluding those with release channels
func (s *Service) ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error) {
	return s.repository.ListCRDs(ctx, excludeChannelInstances)
}

// Get retrieves a single unleash instance by name
func (s *Service) Get(ctx context.Context, name string) (*unleash.Instance, error) {
	return s.repository.Get(ctx, name)
}

// GetCRD retrieves a single unleash CRD by name
func (s *Service) GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	return s.repository.GetCRD(ctx, name)
}

// Create creates a new unleash instance with its database and resources
func (s *Service) Create(ctx context.Context, config *unleash.Config) (*unleashv1.Unleash, error) {
	// Create database
	if err := s.dbManager.CreateDatabase(ctx, config.Name); err != nil {
		return nil, err
	}

	// Create database user
	password, err := s.dbManager.CreateDatabaseUser(ctx, config.Name)
	if err != nil {
		return nil, err
	}

	// Create secret with credentials
	if err := s.dbManager.CreateSecret(ctx, config.Name, password); err != nil {
		return nil, err
	}

	// Create unleash instance in Kubernetes
	if err := s.repository.Create(ctx, config); err != nil {
		return nil, err
	}

	// Retrieve the created CRD to return
	crd, err := s.getCRD(ctx, config.Name)
	if err != nil {
		return nil, err
	}

	s.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation":      "create_unleash",
		"instance":       config.Name,
		"version_source": config.VersionSource(),
	}).Info("Created unleash instance")

	return crd, nil
}

// Update updates an existing unleash instance
func (s *Service) Update(ctx context.Context, config *unleash.Config) (*unleashv1.Unleash, error) {
	// Check what version source was previously configured
	existing, err := s.repository.Get(ctx, config.Name)
	if err != nil {
		return nil, err
	}

	oldSource := existing.VersionSource()
	newSource := config.VersionSource()

	// Update the unleash instance
	if err := s.repository.Update(ctx, config); err != nil {
		return nil, err
	}

	// Retrieve the updated CRD to return
	crd, err := s.getCRD(ctx, config.Name)
	if err != nil {
		return nil, err
	}

	// Log version source changes
	if oldSource != newSource {
		s.logger.WithContext(ctx).WithFields(logrus.Fields{
			"operation": "version_source_change",
			"instance":  config.Name,
			"from":      oldSource,
			"to":        newSource,
		}).Info("Version source changed")
	}

	return crd, nil
}

// Delete deletes an unleash instance and its resources
func (s *Service) Delete(ctx context.Context, name string) error {
	// Delete in reverse order of creation
	if err := s.repository.Delete(ctx, name); err != nil {
		return err
	}

	if err := s.dbManager.DeleteSecret(ctx, name); err != nil {
		return err
	}

	if err := s.dbManager.DeleteDatabaseUser(ctx, name); err != nil {
		return err
	}

	if err := s.dbManager.DeleteDatabase(ctx, name); err != nil {
		return err
	}

	s.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_unleash",
		"instance":  name,
	}).Info("Deleted unleash instance")

	return nil
}

// getCRD is a helper to retrieve the Kubernetes CRD for an instance
func (s *Service) getCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	return s.repository.GetCRD(ctx, name)
}
