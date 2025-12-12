package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mock service for v0 API tests
type MockV0UnleashService struct {
	ListFunc   func(ctx context.Context) ([]*unleash.UnleashInstance, error)
	GetFunc    func(ctx context.Context, name string) (*unleash.UnleashInstance, error)
	CreateFunc func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error)
	UpdateFunc func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error)
	DeleteFunc func(ctx context.Context, name string) error
}

func (m *MockV0UnleashService) List(ctx context.Context) ([]*unleash.UnleashInstance, error) {
	if m.ListFunc != nil {
		return m.ListFunc(ctx)
	}
	return nil, nil
}

func (m *MockV0UnleashService) Get(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
	if m.GetFunc != nil {
		return m.GetFunc(ctx, name)
	}
	return nil, errors.New("not found")
}

func (m *MockV0UnleashService) Create(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, uc)
	}
	return nil, nil
}

func (m *MockV0UnleashService) Update(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
	if m.UpdateFunc != nil {
		return m.UpdateFunc(ctx, uc)
	}
	return nil, nil
}

func (m *MockV0UnleashService) Delete(ctx context.Context, name string) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, name)
	}
	return nil
}

func setupTestHandler(service unleash.IUnleashService) (*Handler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	config := &config.Config{}
	logger := logrus.New()
	handler := NewHandler(config, logger, service)
	return handler, router
}

func TestHealthHandler(t *testing.T) {
	mockService := &MockV0UnleashService{}
	handler, router := setupTestHandler(mockService)
	router.GET("/healthz", handler.HealthHandler)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "OK", w.Body.String())
}

func TestUnleashInstanceMiddleware_Success(t *testing.T) {
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			assert.Equal(t, "test-instance", name)
			return &unleash.UnleashInstance{
				Name: "test-instance",
				ServerInstance: &unleashv1.Unleash{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-instance",
					},
				},
			}, nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.GET("/unleash/:id", handler.UnleashInstanceMiddleware, func(c *gin.Context) {
		instance, exists := c.Get("unleashInstance")
		assert.True(t, exists)
		assert.NotNil(t, instance)
		c.JSON(http.StatusOK, gin.H{"found": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/unleash/test-instance", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUnleashInstanceMiddleware_NotFound(t *testing.T) {
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			return nil, errors.New("instance not found")
		},
	}

	handler, router := setupTestHandler(mockService)
	router.GET("/unleash/:id", handler.UnleashInstanceMiddleware, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"should": "not reach here"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/unleash/nonexistent", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Instance not found", response.Error)
	assert.Contains(t, response.Details, "Unleash instance 'nonexistent' does not exist")
}

func TestUnleashInstancePost_Create_Success(t *testing.T) {
	var capturedConfig *unleash.UnleashConfig
	mockService := &MockV0UnleashService{
		CreateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			capturedConfig = uc
			return &unleashv1.Unleash{
				ObjectMeta: metav1.ObjectMeta{
					Name: uc.Name,
				},
				Spec: unleashv1.UnleashSpec{
					CustomImage: "unleash:5.10.2",
				},
			}, nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "new-instance",
		"custom-version": "5.10.2",
		"enable-federation": true,
		"allowed-teams": "team-a,team-b",
		"log-level": "info",
		"database-pool-max": 5,
		"database-pool-idle-timeout-ms": 2000
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotNil(t, capturedConfig)
	assert.Equal(t, "new-instance", capturedConfig.Name)
	assert.Equal(t, "5.10.2", capturedConfig.CustomVersion)
	assert.True(t, capturedConfig.EnableFederation)
	assert.Equal(t, "team-a,team-b", capturedConfig.AllowedTeams)

	var response UnleashResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "new-instance", response.Unleash.Name)
}

func TestUnleashInstancePost_Update_Success(t *testing.T) {
	var capturedConfig *unleash.UnleashConfig
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			return &unleash.UnleashInstance{
				Name: "existing-instance",
				ServerInstance: &unleashv1.Unleash{
					ObjectMeta: metav1.ObjectMeta{
						Name: "existing-instance",
					},
					Spec: unleashv1.UnleashSpec{
						CustomImage: "unleash:5.10.0",
						Federation: unleashv1.UnleashFederationConfig{
							Enabled:     true,
							SecretNonce: "original-nonce",
						},
						ExtraEnvVars: []corev1.EnvVar{
							{Name: "TEAMS_ALLOWED_TEAMS", Value: "team-x"},
							{Name: "LOG_LEVEL", Value: "warn"},
							{Name: "DATABASE_POOL_MAX", Value: "3"},
							{Name: "DATABASE_POOL_IDLE_TIMEOUT_MS", Value: "1000"},
						},
					},
				},
			}, nil
		},
		UpdateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			capturedConfig = uc
			return &unleashv1.Unleash{
				ObjectMeta: metav1.ObjectMeta{
					Name: uc.Name,
				},
				Spec: unleashv1.UnleashSpec{
					CustomImage: "unleash:5.11.0",
				},
			}, nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/:id/edit", handler.UnleashInstanceMiddleware, handler.UnleashInstancePost)

	payload := `{
		"custom-version": "5.11.0",
		"allowed-teams": "team-y,team-z",
		"log-level": "debug"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/existing-instance/edit", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotNil(t, capturedConfig)
	assert.Equal(t, "existing-instance", capturedConfig.Name)
	assert.Equal(t, "5.11.0", capturedConfig.CustomVersion)
	assert.Equal(t, "original-nonce", capturedConfig.FederationNonce, "Federation nonce should be preserved")
}

func TestUnleashInstancePost_ValidationError_InvalidName(t *testing.T) {
	mockService := &MockV0UnleashService{}
	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "Invalid_Name!",
		"custom-version": "5.10.2"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Validation failed", response.Error)
}

func TestUnleashInstancePost_ValidationError_InvalidLogLevel(t *testing.T) {
	mockService := &MockV0UnleashService{}
	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "test-instance",
		"log-level": "invalid-level"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Validation failed", response.Error)
}

func TestUnleashInstancePost_InvalidJSON(t *testing.T) {
	mockService := &MockV0UnleashService{}
	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{invalid json`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Invalid request body", response.Error)
}

func TestUnleashInstancePost_ServiceError_Create(t *testing.T) {
	mockService := &MockV0UnleashService{
		CreateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			return nil, errors.New("kubernetes API error")
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "test-instance",
		"custom-version": "5.10.2"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Error persisting Unleash instance", response.Error)
}

func TestUnleashInstancePost_UnleashError(t *testing.T) {
	mockService := &MockV0UnleashService{
		CreateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			return nil, &unleash.UnleashError{
				Err:    errors.New("internal error"),
				Reason: "failed to create database",
			}
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "test-instance",
		"custom-version": "5.10.2"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Error persisting Unleash instance", response.Error)
	assert.Equal(t, "failed to create database", response.Details)
}

func TestUnleashInstancePost_MergesTeamsAndNamespaces(t *testing.T) {
	var capturedConfig *unleash.UnleashConfig
	mockService := &MockV0UnleashService{
		CreateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			capturedConfig = uc
			return &unleashv1.Unleash{
				ObjectMeta: metav1.ObjectMeta{Name: uc.Name},
			}, nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	payload := `{
		"name": "test-instance",
		"allowed-teams": "team-a,team-b",
		"allowed-namespaces": "team-b,team-c"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotNil(t, capturedConfig)
	// Should merge and sort: team-a, team-b, team-c
	assert.Equal(t, "team-a,team-b,team-c", capturedConfig.AllowedTeams)
	assert.Equal(t, "team-a,team-b,team-c", capturedConfig.AllowedNamespaces)
}

func TestUnleashInstanceDelete_Success(t *testing.T) {
	var deletedName string
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			return &unleash.UnleashInstance{
				Name: name,
				ServerInstance: &unleashv1.Unleash{
					ObjectMeta: metav1.ObjectMeta{Name: name},
				},
			}, nil
		},
		DeleteFunc: func(ctx context.Context, name string) error {
			deletedName = name
			return nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.DELETE("/unleash/:id", handler.UnleashInstanceMiddleware, handler.UnleashInstanceDelete)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/unleash/test-instance", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "test-instance", deletedName)
	assert.Empty(t, w.Body.String())
}

func TestUnleashInstanceDelete_NotFound(t *testing.T) {
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			return nil, errors.New("not found")
		},
	}

	handler, router := setupTestHandler(mockService)
	router.DELETE("/unleash/:id", handler.UnleashInstanceMiddleware, handler.UnleashInstanceDelete)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/unleash/nonexistent", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Instance not found", response.Error)
}

func TestUnleashInstanceDelete_ServiceError(t *testing.T) {
	mockService := &MockV0UnleashService{
		GetFunc: func(ctx context.Context, name string) (*unleash.UnleashInstance, error) {
			return &unleash.UnleashInstance{
				Name: name,
				ServerInstance: &unleashv1.Unleash{
					ObjectMeta: metav1.ObjectMeta{Name: name},
				},
			}, nil
		},
		DeleteFunc: func(ctx context.Context, name string) error {
			return errors.New("kubernetes API error")
		},
	}

	handler, router := setupTestHandler(mockService)
	router.DELETE("/unleash/:id", handler.UnleashInstanceMiddleware, handler.UnleashInstanceDelete)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/unleash/test-instance", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "Error deleting Unleash instance", response.Error)
}

func TestUnleashInstancePost_DefaultValues(t *testing.T) {
	var capturedConfig *unleash.UnleashConfig
	mockService := &MockV0UnleashService{
		CreateFunc: func(ctx context.Context, uc *unleash.UnleashConfig) (*unleashv1.Unleash, error) {
			capturedConfig = uc
			return &unleashv1.Unleash{
				ObjectMeta: metav1.ObjectMeta{Name: uc.Name},
			}, nil
		},
	}

	handler, router := setupTestHandler(mockService)
	router.POST("/unleash/new", handler.UnleashInstancePost)

	// Minimal payload - should set defaults
	payload := `{
		"name": "test-instance"
	}`

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash/new", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotNil(t, capturedConfig)
	assert.Equal(t, "test-instance", capturedConfig.Name)
	// Should have default values
	assert.NotEmpty(t, capturedConfig.CustomVersion, "Should set default unleash version")
	assert.Equal(t, "warn", capturedConfig.LogLevel, "Should set default log level")
	assert.Equal(t, 3, capturedConfig.DatabasePoolMax, "Should set default pool max")
	assert.Equal(t, 1000, capturedConfig.DatabasePoolIdleTimeoutMs, "Should set default pool timeout")
	assert.NotEmpty(t, capturedConfig.FederationNonce, "Should generate federation nonce")
}
