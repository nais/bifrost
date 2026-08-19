package unleash

import (
	"context"

	unleashv1 "github.com/nais/unleasherator/api/v1"
)

// UpdateOptions carries write preconditions for Repository.Update.
//
// It exists because Update renders a whole instance from a Config the caller
// built earlier, from data it read even earlier. Without a precondition the
// write lands unconditionally and whatever another writer stored in between is
// gone with no error to notice it by.
type UpdateOptions struct {
	// ExpectedResourceVersion, when non-empty, makes the write conditional on
	// the instance still being at that version; if it is not, the update fails
	// with a Kubernetes Conflict the caller can surface or retry.
	//
	// Callers that read an instance, derive part of the new config from what
	// they read, and then write must set it to the version they read. Empty
	// means unconditional — last writer wins — and is only safe when nothing in
	// the config was derived from a prior read.
	ExpectedResourceVersion string
}

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
	Update(ctx context.Context, config *Config, opts UpdateOptions) error

	// Delete removes an Unleash instance
	Delete(ctx context.Context, name string) error
}
