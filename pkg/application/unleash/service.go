package unleash

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// cleanupTimeout bounds best-effort rollback/teardown, which runs on a context
// detached from the request so it completes even if the caller disconnected.
const cleanupTimeout = 5 * time.Minute

// IService defines the interface for unleash instance management operations
type IService interface {
	List(ctx context.Context, excludeChannelInstances bool) ([]*unleash.Instance, error)
	ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error)
	Get(ctx context.Context, name string) (*unleash.Instance, error)
	GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error)
	Create(ctx context.Context, config *unleash.Config) (*unleashv1.Unleash, error)
	Update(ctx context.Context, config *unleash.Config, opts unleash.UpdateOptions) (*unleashv1.Unleash, error)
	Delete(ctx context.Context, name string) error
}

// DatabaseManager defines the interface for database operations
// DatabaseManager provisions the Cloud SQL resources behind an instance.
//
// The Create* methods return an "owned" flag that reports whether *this call*
// brought the resource into existence. Rollback keys off that flag, so a
// resource that already existed is never torn down by an operation that merely
// encountered it. The flag is set as soon as the create call is accepted —
// before the long-running operation is awaited — because a cancelled or
// timed-out wait still leaves the resource to be cleaned up.
type DatabaseManager interface {
	CreateDatabase(ctx context.Context, name string) (owned bool, err error)
	CreateDatabaseUser(ctx context.Context, name string) (password string, owned bool, err error)
	CreateSecret(ctx context.Context, name string, password string) (owned bool, err error)
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

	// instanceLocks serializes API-driven lifecycle operations per instance name.
	//
	// The existence pre-check in the handler closes the sequential duplicate
	// window, but not the concurrent one: two POSTs for the same name both pass
	// the check, and whichever loses a create race rolls back resources the
	// winner is about to ship. bifrost runs a single replica, so an in-process
	// lock is sufficient; if it is ever scaled out this must become a lease or
	// a database advisory lock.
	//
	// Scope: this covers calls that go through Service. The migration and
	// channel reconcilers write through unleash.Repository directly and are not
	// serialized by it, so a reconciler write can land at any point during an
	// API update.
	//
	// Update now carries the caller's resourceVersion as a precondition, which
	// closes one of the two orderings: a reconciler write landing *between* the
	// caller's read and its write now fails with a Conflict instead of silently
	// overwriting. The reverse ordering is still lossy — the reconcilers pass no
	// precondition of their own, so a reconciler write *after* a successful API
	// update lands unconditionally and reverts it, and the caller has already
	// been told 200. Fixing that means giving the reconcilers a precondition
	// too, which is a separate decision about how a failed one-shot batch should
	// behave.
	instanceLocks sync.Map // instance name -> *sync.Mutex
}

// lockInstance serializes operations on one instance name and returns the
// release function.
func (s *Service) lockInstance(name string) func() {
	value, _ := s.instanceLocks.LoadOrStore(name, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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

// Create creates a new unleash instance with its database and resources.
//
// Provisioning spans Cloud SQL (database + user) and Kubernetes (secret + CRD)
// with no cross-system transaction, so any step failing partway used to orphan
// the resources created before it. Create now rolls back everything it created
// on failure, so a failed create leaves no orphaned database, user, secret, or
// CRD behind. Rollback runs on a detached, bounded context so it completes even
// if the request context was cancelled.
func (s *Service) Create(ctx context.Context, config *unleash.Config) (crd *unleashv1.Unleash, err error) {
	name := config.Name

	// Serialize with any other lifecycle operation on this name. Two concurrent
	// creates would otherwise both pass the handler's existence check, and the
	// one that loses the database race would roll back resources the winner is
	// about to ship.
	defer s.lockInstance(name)()

	var dbCreated, userCreated, secretCreated, crdCreated bool
	defer func() {
		if err == nil {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		log := s.logger.WithContext(ctx).WithField("instance", name)
		log.WithError(err).Warn("Create failed, rolling back partially-created resources")

		// Reverse order of creation; drop the database before the user it owns.
		if crdCreated {
			if e := s.repository.Delete(cctx, name); e != nil && !apierrors.IsNotFound(e) {
				log.WithError(e).Error("Rollback: failed to delete unleash CRD")
			}
		}
		if secretCreated {
			if e := s.dbManager.DeleteSecret(cctx, name); e != nil {
				log.WithError(e).Error("Rollback: failed to delete credentials secret")
			}
		}
		if dbCreated {
			if e := s.dbManager.DeleteDatabase(cctx, name); e != nil {
				log.WithError(e).Error("Rollback: failed to delete database")
			}
		}
		if userCreated {
			if e := s.dbManager.DeleteDatabaseUser(cctx, name); e != nil {
				log.WithError(e).Error("Rollback: failed to delete database user")
			}
		}
	}()

	// Each step reports whether it created the resource. Assign the flag before
	// checking the error: a step that was accepted but whose wait failed or was
	// cancelled still owns the resource and must be rolled back.

	// Create database
	if dbCreated, err = s.dbManager.CreateDatabase(ctx, name); err != nil {
		return nil, err
	}

	// Create database user
	var password string
	if password, userCreated, err = s.dbManager.CreateDatabaseUser(ctx, name); err != nil {
		return nil, err
	}

	// Create secret with credentials
	if secretCreated, err = s.dbManager.CreateSecret(ctx, name, password); err != nil {
		return nil, err
	}

	// Create unleash instance in Kubernetes
	if err = s.repository.Create(ctx, config); err != nil {
		return nil, err
	}
	crdCreated = true

	// Retrieve the created CRD to return
	if crd, err = s.getCRD(ctx, name); err != nil {
		return nil, err
	}

	s.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation":      "create_unleash",
		"instance":       name,
		"version_source": config.VersionSource(),
	}).Info("Created unleash instance")

	return crd, nil
}

// Update writes a new configuration for an existing instance.
//
// opts.ExpectedResourceVersion should carry the version the caller read the
// instance at, since callers derive parts of config (version source, federation
// nonce, allowed teams) from that read. A write landing in between then fails
// with a Conflict rather than reverting it.
func (s *Service) Update(ctx context.Context, config *unleash.Config, opts unleash.UpdateOptions) (*unleashv1.Unleash, error) {
	// Serialize with any create or delete in flight for this instance.
	defer s.lockInstance(config.Name)()

	// Check what version source was previously configured
	existing, err := s.repository.Get(ctx, config.Name)
	if err != nil {
		return nil, err
	}

	oldSource := existing.VersionSource()
	newSource := config.VersionSource()

	// Update the unleash instance
	if err := s.repository.Update(ctx, config, opts); err != nil {
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

// Delete deletes an unleash instance and its resources.
//
// Deletion is best-effort and idempotent: every step is attempted regardless of
// earlier failures (errors are aggregated), and each underlying operation treats
// an already-absent resource as success. This means a transient failure in one
// step no longer skips the remaining teardown and orphans the database/user, and
// the call can be safely retried to reap anything left behind.
func (s *Service) Delete(ctx context.Context, name string) error {
	// Serialize with any create or update in flight for this instance.
	defer s.lockInstance(name)()

	var errs []error

	// Delete the CRD first to stop the workload, then its credentials secret.
	if err := s.repository.Delete(ctx, name); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	if err := s.dbManager.DeleteSecret(ctx, name); err != nil {
		errs = append(errs, err)
	}
	// Delete the database before the user to avoid dependency errors
	// (PostgreSQL won't let you drop a user that owns database objects).
	if err := s.dbManager.DeleteDatabase(ctx, name); err != nil {
		errs = append(errs, err)
	}
	if err := s.dbManager.DeleteDatabaseUser(ctx, name); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
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
