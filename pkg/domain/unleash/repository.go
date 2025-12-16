package unleash

import (
	"context"

	unleashv1 "github.com/nais/unleasherator/api/v1"
)

// Repository defines the contract for Unleash instance persistence operations
type Repository interface {
	// List returns all Unleash instances, optionally excluding those with release channels
	List(ctx context.Context, excludeChannelInstances bool) ([]*Instance, error)

	// ListCRDs returns all Unleash CRDs, optionally excluding those with release channels
	ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error)

	// Get retrieves a single Unleash instance by name
	Get(ctx context.Context, name string) (*Instance, error)

	// GetCRD retrieves a single Unleash CRD by name
	GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error)

	// Create creates a new Unleash instance
	Create(ctx context.Context, config *Config) error

	// Update updates an existing Unleash instance
	Update(ctx context.Context, config *Config) error

	// Delete removes an Unleash instance
	Delete(ctx context.Context, name string) error
}
