package unleash

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
)

// fakeDBManager records calls and can be told to fail a specific step.
type fakeDBManager struct {
	failCreateDB, failCreateUser, failCreateSecret bool
	failDeleteDB                                   bool

	calls []string
}

func (f *fakeDBManager) record(c string)      { f.calls = append(f.calls, c) }
func (f *fakeDBManager) called(c string) bool { return slices.Contains(f.calls, c) }

func (f *fakeDBManager) CreateDatabase(_ context.Context, _ string) error {
	f.record("CreateDatabase")
	if f.failCreateDB {
		return errors.New("boom db")
	}
	return nil
}

func (f *fakeDBManager) CreateDatabaseUser(_ context.Context, _ string) (string, error) {
	f.record("CreateDatabaseUser")
	if f.failCreateUser {
		return "", errors.New("boom user")
	}
	return "pw", nil
}

func (f *fakeDBManager) CreateSecret(_ context.Context, _ string, _ string) error {
	f.record("CreateSecret")
	if f.failCreateSecret {
		return errors.New("boom secret")
	}
	return nil
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
func (f *fakeRepo) Update(context.Context, *unleash.Config) error { f.record("Update"); return nil }
func (f *fakeRepo) Delete(context.Context, string) error          { f.record("Delete"); return nil }
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
