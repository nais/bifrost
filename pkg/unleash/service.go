package unleash

import (
	"context"

	unleashv1 "github.com/nais/unleasherator/api/v1"
	admin "google.golang.org/api/sqladmin/v1beta4"
)

// IUnleashService defines the interface for managing Unleash instances
// This interface is used by v0 handlers for backward compatibility
// The implementation is in adapter.go which wraps the new application service
type IUnleashService interface {
	List(ctx context.Context) ([]*UnleashInstance, error)
	Get(ctx context.Context, name string) (*UnleashInstance, error)
	Create(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error)
	Update(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error)
	Delete(ctx context.Context, name string) error
}

// ISQLDatabasesService interface for Google Cloud SQL database operations
// Used for mocking in tests
type ISQLDatabasesService interface {
	Get(project string, instance string, database string) *admin.DatabasesGetCall
	Insert(project string, instance string, database *admin.Database) *admin.DatabasesInsertCall
	Delete(project string, instance string, database string) *admin.DatabasesDeleteCall
}

// ISQLUsersService interface for Google Cloud SQL user operations
// Used for mocking in tests
type ISQLUsersService interface {
	Get(project string, instance string, name string) *admin.UsersGetCall
	Insert(project string, instance string, user *admin.User) *admin.UsersInsertCall
	Delete(project string, instance string) *admin.UsersDeleteCall
}
