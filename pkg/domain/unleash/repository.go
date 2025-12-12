package unleash

import (
	"context"
)

// Repository defines the contract for Unleash instance persistence operations
type Repository interface {
	// List returns all Unleash instances, optionally excluding those with release channels
	List(ctx context.Context, excludeChannelInstances bool) ([]*Instance, error)

	// Get retrieves a single Unleash instance by name
	Get(ctx context.Context, name string) (*Instance, error)

	// Create creates a new Unleash instance
	Create(ctx context.Context, config *Config) error

	// Update updates an existing Unleash instance
	Update(ctx context.Context, config *Config) error

	// Delete removes an Unleash instance
	Delete(ctx context.Context, name string) error
}
