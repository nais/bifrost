package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MockUnleashService struct {
	c         *config.Config
	Instances []*unleash.UnleashInstance
}

func (s *MockUnleashService) List(ctx context.Context) ([]*unleash.UnleashInstance, error) {
	return s.Instances, nil
}

func (s *MockUnleashService) Get(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
	for _, instance := range s.Instances {
		if instance.Name == name {
			return instance, nil
		}
	}

	return nil, fmt.Errorf("instance not found")
}

func (s *MockUnleashService) Create(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
	spec := unleash.UnleashDefinition(s.c, uc)

	s.Instances = append(s.Instances, &unleash.UnleashInstance{
		Name:           uc.Name,
		CreatedAt:      metav1.Now(),
		ServerInstance: &spec,
	})

	return s.Instances[len(s.Instances)-1].ServerInstance, nil
}

func (s *MockUnleashService) Update(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
	spec := unleash.UnleashDefinition(s.c, uc)

	for _, instance := range s.Instances {
		if instance.Name == uc.Name {
			instance.ServerInstance = &spec
			return instance.ServerInstance, nil
		}
	}

	return nil, fmt.Errorf("instance not found")
}

func (s *MockUnleashService) Delete(ctx context.Context, name string) error {
	for i, instance := range s.Instances {
		if instance.Name == name {
			s.Instances = append(s.Instances[:i], s.Instances[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("instance not found")
}

func TestHealthzRoute(t *testing.T) {
	config := &config.Config{}
	logger := logrus.New()
	service := &MockUnleashService{c: config}

	router := setupRouter(config, logger, service, nil, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "OK", w.Body.String())
}

func newUnleashRoute() (c *config.Config, service *MockUnleashService, router *gin.Engine) {
	c = &config.Config{}

	unleash1 := unleash.UnleashDefinition(c, &unleash.UnleashConfig{
		Name:                      "team-a",
		CustomVersion:             "v1.2.3-00000000-000000-abcd1234",
		EnableFederation:          true,
		FederationNonce:           "abc123",
		AllowedTeams:              "team-a,team-b",
		AllowedNamespaces:         "ns-a,ns-b",
		AllowedClusters:           "cluster-a,cluster-b",
		LogLevel:                  "debug",
		DatabasePoolMax:           10,
		DatabasePoolIdleTimeoutMs: 100,
	})
	unleash1.Status = unleashv1.UnleashStatus{
		Version: "1.2.3",
	}
	unleash2 := unleash.UnleashDefinition(c, &unleash.UnleashConfig{
		Name:                      "team-b",
		CustomVersion:             "",
		EnableFederation:          false,
		FederationNonce:           "",
		AllowedTeams:              "",
		AllowedNamespaces:         "",
		AllowedClusters:           "",
		LogLevel:                  "warn",
		DatabasePoolMax:           3,
		DatabasePoolIdleTimeoutMs: 1000,
	})
	unleash2.Status = unleashv1.UnleashStatus{
		Version: "4.5.6",
	}

	logger := logrus.New()
	service = &MockUnleashService{
		c: c,
		Instances: []*unleash.UnleashInstance{
			{
				Name:           "team-a",
				CreatedAt:      metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				ServerInstance: &unleash1,
			},
			{
				Name:           "team-b",
				CreatedAt:      metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
				ServerInstance: &unleash2,
			},
		},
	}

	router = setupRouter(c, logger, service, nil, nil)

	return
}

// JSON API Tests - establishing baseline contract for API-only refactoring

func TestJSONAPI_CreateUnleashInstance_Success(t *testing.T) {
	_, service, router := newUnleashRoute()

	payload := `{
		"name": "test-instance",
		"custom-version": "v5.10.2-20240329-070801-0180a96",
		"enable-federation": true,
		"allowed-teams": "team-x,team-y",
		"allowed-namespaces": "ns-x,ns-y",
		"allowed-clusters": "dev-gcp,prod-gcp",
		"log-level": "info",
		"database-pool-max": 5,
		"database-pool-idle-timeout-ms": 2000
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"name":"test-instance"`)
	assert.Contains(t, w.Body.String(), `"kind":"Unleash"`)
	assert.Equal(t, 3, len(service.Instances))
	assert.Equal(t, "test-instance", service.Instances[2].Name)
}

func TestJSONAPI_CreateUnleashInstance_ValidationError(t *testing.T) {
	_, service, router := newUnleashRoute()

	// Missing required name field
	payload := `{
		"custom-version": "v5.10.2",
		"enable-federation": true
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 400, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "validation")
	assert.Equal(t, 2, len(service.Instances)) // Should not create instance
}

func TestJSONAPI_CreateUnleashInstance_InvalidJSON(t *testing.T) {
	_, _, router := newUnleashRoute()

	payload := `{invalid json`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 400, w.Code) // Invalid JSON returns 400 bad request
	assert.Contains(t, w.Body.String(), "Invalid request body")
}

func TestJSONAPI_UpdateUnleashInstance_Success(t *testing.T) {
	_, service, router := newUnleashRoute()

	payload := `{
		"custom-version": "v5.11.0-20240401-080000-xyz123",
		"allowed-teams": "team-z",
		"log-level": "warn",
		"database-pool-max": 8
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/team-a/edit", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"name":"team-a"`)
	assert.Contains(t, w.Body.String(), `"kind":"Unleash"`)

	// Verify the instance was updated
	updated := service.Instances[0]
	assert.Equal(t, "team-a", updated.Name)
	assert.Contains(t, updated.ServerInstance.Spec.CustomImage, "v5.11.0-20240401-080000-xyz123")
}

func TestJSONAPI_UpdateUnleashInstance_NotFound(t *testing.T) {
	_, _, router := newUnleashRoute()

	payload := `{
		"allowed-teams": "team-z"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/nonexistent/edit", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 404, w.Code) // Now returns proper 404 JSON error
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "Instance not found")
}

func TestJSONAPI_UpdateUnleashInstance_ValidationError(t *testing.T) {
	_, _, router := newUnleashRoute()

	// Invalid log-level value
	payload := `{
		"log-level": "invalid-level"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/team-a/edit", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 400, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "validation")
}

func TestJSONAPI_DeleteUnleashInstance_Success(t *testing.T) {
	_, service, router := newUnleashRoute()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/unleash/team-a", nil)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, 204, w.Code)                         // No content on successful delete
	assert.Equal(t, 1, len(service.Instances))           // Verify instance was deleted
	assert.Equal(t, "team-b", service.Instances[0].Name) // Only team-b remains
}

func TestJSONAPI_DeleteUnleashInstance_NotFound(t *testing.T) {
	_, _, router := newUnleashRoute()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/unleash/nonexistent", nil)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// After refactoring, we expect:
	// assert.Equal(t, 404, w.Code)
	// assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	// For now, just document current behavior
	assert.Equal(t, 404, w.Code)
}
