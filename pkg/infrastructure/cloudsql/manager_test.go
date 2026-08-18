package cloudsql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nais/bifrost/pkg/config"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/option"
	admin "google.golang.org/api/sqladmin/v1beta4"
)

// newTestManager wires a Manager against an httptest server standing in for the
// Cloud SQL Admin API. The service interfaces expose concrete google-api call
// types, so a real client pointed at a fake endpoint is the only way to
// exercise the status-code handling that the ownership contract rests on.
func newTestManager(t *testing.T, handler http.HandlerFunc) *Manager {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	svc, err := admin.NewService(context.Background(),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("building sqladmin service: %v", err)
	}

	logger := logrus.New()
	logger.SetOutput(nopWriter{})

	return NewManager(svc.Databases, svc.Users, svc.Operations, nil, &config.Config{}, logger)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// A database that already exists is not ours. Reporting it as created is what
// let a rollback drop the live database of an instance this call never created.
func TestCreateDatabase_ExistingDatabaseIsNotOwned(t *testing.T) {
	m := newTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"database already exists"}}`))
	})

	owned, err := m.CreateDatabase(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("an existing database is not an error, got %v", err)
	}
	if owned {
		t.Fatal("a pre-existing database must not be reported as owned")
	}
}

// A create the API accepted owns the resource even if the wait then fails: the
// operation still completes server-side, so skipping rollback orphans it.
func TestCreateDatabase_OwnedWhenOperationWaitFails(t *testing.T) {
	m := newTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/operations/") {
			// Poll reports the operation finished with an error.
			_, _ = w.Write([]byte(`{"name":"op-1","status":"DONE","error":{"errors":[{"message":"insert failed"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"op-1","status":"PENDING"}`))
	})

	owned, err := m.CreateDatabase(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected an error when the operation fails")
	}
	if !owned {
		t.Fatal("a database whose create was accepted must be owned so rollback cleans it up")
	}
}

func TestCreateDatabase_OwnedOnSuccess(t *testing.T) {
	m := newTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"op-1","status":"DONE"}`))
	})

	owned, err := m.CreateDatabase(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !owned {
		t.Fatal("a database this call created must be owned")
	}
}

// A genuine failure owns nothing — there is no resource to roll back.
func TestCreateDatabase_NotOwnedWhenInsertRejected(t *testing.T) {
	m := newTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"nope"}}`))
	})

	owned, err := m.CreateDatabase(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected an error when the insert is rejected")
	}
	if owned {
		t.Fatal("a rejected insert must not be reported as owned")
	}
}

// An existing user must fail loudly rather than silently reusing credentials
// the caller cannot know, and it must not be reported as owned.
func TestCreateDatabaseUser_ExistingUserIsNotOwned(t *testing.T) {
	m := newTestManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"user already exists"}}`))
	})

	_, owned, err := m.CreateDatabaseUser(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected an error for an existing user")
	}
	if owned {
		t.Fatal("a pre-existing user must not be reported as owned")
	}
}
