package unleash

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeDBManager records calls and can be told to fail a specific step.
type fakeDBManager struct {
	failCreateDB, failCreateUser, failCreateSecret bool
	failDeleteDB                                   bool

	// dbExists and secretExists model resources that predate this operation.
	// Without them the fake cannot express "already there", which is exactly
	// the state that must never be rolled back.
	dbExists, secretExists bool

	// failWaitCreateDB models a create that the API accepted but whose
	// long-running operation then failed or was cancelled: the resource may
	// exist server-side, so it must still be rolled back.
	failWaitCreateDB bool

	calls []string
}

func (f *fakeDBManager) record(c string)      { f.calls = append(f.calls, c) }
func (f *fakeDBManager) called(c string) bool { return slices.Contains(f.calls, c) }

func (f *fakeDBManager) CreateDatabase(_ context.Context, _ string) (bool, error) {
	f.record("CreateDatabase")
	if f.failCreateDB {
		return false, errors.New("boom db")
	}
	if f.dbExists {
		// Already there: not ours, so not rollback-eligible.
		return false, nil
	}
	if f.failWaitCreateDB {
		// Accepted, then the wait failed — we own it even though it errored.
		return true, errors.New("boom db wait")
	}
	return true, nil
}

func (f *fakeDBManager) CreateDatabaseUser(_ context.Context, _ string) (string, bool, error) {
	f.record("CreateDatabaseUser")
	if f.failCreateUser {
		return "", false, errors.New("boom user")
	}
	return "pw", true, nil
}

func (f *fakeDBManager) CreateSecret(_ context.Context, _ string, _ string) (bool, error) {
	f.record("CreateSecret")
	if f.failCreateSecret {
		return false, errors.New("boom secret")
	}
	if f.secretExists {
		return false, nil
	}
	return true, nil
}

func (f *fakeDBManager) DeleteDatabase(_ context.Context, _ string) error {
	f.record("DeleteDatabase")
	if f.failDeleteDB {
		return errors.New("boom delete db")
	}
	return nil
}

func (f *fakeDBManager) DeleteDatabaseUser(_ context.Context, _ string) error {
	f.record("DeleteDatabaseUser")
	return nil
}

func (f *fakeDBManager) DeleteSecret(_ context.Context, _ string) error {
	f.record("DeleteSecret")
	return nil
}

// fakeRepo records calls and can fail Create.
type fakeRepo struct {
	failCreate bool
	calls      []string

	// updateOpts captures the preconditions Update was called with, so a test
	// can assert the caller's resourceVersion actually reaches the repository.
	updateOpts unleash.UpdateOptions
	// updateErr, when set, is what Update returns.
	updateErr error
}

func (f *fakeRepo) record(c string)      { f.calls = append(f.calls, c) }
func (f *fakeRepo) called(c string) bool { return slices.Contains(f.calls, c) }

func (f *fakeRepo) List(context.Context, bool) ([]*unleash.Instance, error)     { return nil, nil }
func (f *fakeRepo) ListCRDs(context.Context, bool) ([]unleashv1.Unleash, error) { return nil, nil }
func (f *fakeRepo) Get(context.Context, string) (*unleash.Instance, error) {
	return &unleash.Instance{}, nil
}

func (f *fakeRepo) GetCRD(context.Context, string) (*unleashv1.Unleash, error) {
	return &unleashv1.Unleash{}, nil
}
func (f *fakeRepo) Update(_ context.Context, _ *unleash.Config, opts unleash.UpdateOptions) error {
	f.record("Update")
	f.updateOpts = opts
	return f.updateErr
}

func (f *fakeRepo) Delete(context.Context, string) error { f.record("Delete"); return nil }
func (f *fakeRepo) Create(_ context.Context, _ *unleash.Config) error {
	f.record("Create")
	if f.failCreate {
		return errors.New("boom crd")
	}
	return nil
}

func newTestService(repo *fakeRepo, db *fakeDBManager) *Service {
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	return NewService(repo, db, &config.Config{}, logger)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestCreate_RollsBackOnCRDFailure(t *testing.T) {
	repo := &fakeRepo{failCreate: true}
	db := &fakeDBManager{}
	svc := newTestService(repo, db)

	_, err := svc.Create(context.Background(), &unleash.Config{Name: "team-a"})
	if err == nil {
		t.Fatal("expected error when CRD create fails")
	}

	// All three DB-side resources were created, so all three must be rolled back.
	for _, want := range []string{"DeleteDatabase", "DeleteDatabaseUser", "DeleteSecret"} {
		if !db.called(want) {
			t.Errorf("rollback did not call %s (calls: %v)", want, db.calls)
		}
	}
}

// The data-loss case: a create that runs against resources it did not create
// must never delete them when it rolls back. Before the ownership flags, a 409
// from Cloud SQL was reported as success and the rollback dropped the live
// database of a running instance.
func TestCreate_RollbackLeavesPreExistingResourcesAlone(t *testing.T) {
	repo := &fakeRepo{failCreate: true}
	db := &fakeDBManager{dbExists: true, secretExists: true}
	svc := newTestService(repo, db)

	_, err := svc.Create(context.Background(), &unleash.Config{Name: "team-a"})
	if err == nil {
		t.Fatal("expected error when CRD create fails")
	}

	if db.called("DeleteDatabase") {
		t.Errorf("rollback dropped a database it did not create (calls: %v)", db.calls)
	}
	if db.called("DeleteSecret") {
		t.Errorf("rollback deleted a secret it did not create (calls: %v)", db.calls)
	}
	// The user was created by this call, so it is ours to clean up.
	if !db.called("DeleteDatabaseUser") {
		t.Errorf("rollback did not clean up the user it created (calls: %v)", db.calls)
	}
}

// A create the API accepted but whose long-running operation failed or was
// cancelled still owns the resource: the operation completes server-side, so
// skipping rollback would orphan it.
func TestCreate_RollsBackResourceWhoseWaitFailed(t *testing.T) {
	repo := &fakeRepo{}
	db := &fakeDBManager{failWaitCreateDB: true}
	svc := newTestService(repo, db)

	_, err := svc.Create(context.Background(), &unleash.Config{Name: "team-a"})
	if err == nil {
		t.Fatal("expected error when the create operation wait fails")
	}

	if !db.called("DeleteDatabase") {
		t.Errorf("rollback skipped a database whose create was accepted (calls: %v)", db.calls)
	}
}

func TestCreate_RollsBackOnSecretFailure(t *testing.T) {
	repo := &fakeRepo{}
	db := &fakeDBManager{failCreateSecret: true}
	svc := newTestService(repo, db)

	_, err := svc.Create(context.Background(), &unleash.Config{Name: "team-a"})
	if err == nil {
		t.Fatal("expected error when secret create fails")
	}

	// DB + user were created and must be rolled back; the CRD was never created.
	if !db.called("DeleteDatabase") || !db.called("DeleteDatabaseUser") {
		t.Errorf("expected db + user rollback, calls: %v", db.calls)
	}
	if repo.called("Delete") {
		t.Errorf("CRD was never created; it must not be deleted in rollback")
	}
}

func TestCreate_NoRollbackOnSuccess(t *testing.T) {
	repo := &fakeRepo{}
	db := &fakeDBManager{}
	svc := newTestService(repo, db)

	if _, err := svc.Create(context.Background(), &unleash.Config{Name: "team-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, unwanted := range []string{"DeleteDatabase", "DeleteDatabaseUser", "DeleteSecret"} {
		if db.called(unwanted) {
			t.Errorf("successful create must not roll back (%s called)", unwanted)
		}
	}
}

func TestDelete_BestEffortContinuesAfterFailure(t *testing.T) {
	repo := &fakeRepo{}
	db := &fakeDBManager{failDeleteDB: true}
	svc := newTestService(repo, db)

	err := svc.Delete(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected aggregated error when a delete step fails")
	}
	// The user delete must still run even though the database delete failed.
	if !db.called("DeleteDatabaseUser") {
		t.Errorf("delete aborted early; DeleteDatabaseUser not called (calls: %v)", db.calls)
	}
}

// Two concurrent creates for the same name must not interleave: the loser of a
// create race would otherwise roll back resources the winner is shipping.
func TestCreate_SerializesConcurrentCreatesForSameName(t *testing.T) {
	repo := &fakeRepo{}
	db := &fakeDBManager{}
	svc := newTestService(repo, db)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Create(context.Background(), &unleash.Config{Name: "team-a"})
		}()
	}
	wg.Wait()

	// Under the lock every create runs to completion before the next starts, so
	// the call log is a whole number of intact sequences. Without it the fake's
	// slice append races and the sequences interleave.
	if len(db.calls)%3 != 0 {
		t.Fatalf("expected whole create sequences, got %v", db.calls)
	}
	for i := 0; i < len(db.calls); i += 3 {
		got := db.calls[i : i+3]
		want := []string{"CreateDatabase", "CreateDatabaseUser", "CreateSecret"}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("create sequences interleaved at %d: %v", i, db.calls)
			}
		}
	}
}

// The precondition is the caller's, not the service's: Service re-reads the
// instance for its own logging, and using that read would only protect the
// microseconds it owns rather than the window the caller has been holding.
func TestUpdate_PassesCallerPreconditionToRepository(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, &fakeDBManager{})

	_, err := svc.Update(context.Background(), &unleash.Config{Name: "team-a"},
		unleash.UpdateOptions{ExpectedResourceVersion: "42"})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if repo.updateOpts.ExpectedResourceVersion != "42" {
		t.Fatalf("ExpectedResourceVersion = %q, want 42", repo.updateOpts.ExpectedResourceVersion)
	}
}

// The handler decides what a conflict means for the client, so the service must
// hand it back recognisable rather than folding it into a generic failure.
func TestUpdate_ReturnsConflictUnwrapped(t *testing.T) {
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "unleash.nais.io", Resource: "unleashes"}, "team-a", errors.New("modified"))
	repo := &fakeRepo{updateErr: conflict}
	svc := newTestService(repo, &fakeDBManager{})

	_, err := svc.Update(context.Background(), &unleash.Config{Name: "team-a"},
		unleash.UpdateOptions{ExpectedResourceVersion: "42"})
	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want a Kubernetes conflict", err)
	}
}
