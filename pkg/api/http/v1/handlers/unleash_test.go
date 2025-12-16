package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	domainUnleash "github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MockDatabaseManager implements the DatabaseManager interface for testing
type MockDatabaseManager struct{}

func (m *MockDatabaseManager) CreateDatabase(ctx context.Context, name string) error {
	return nil
}

func (m *MockDatabaseManager) CreateDatabaseUser(ctx context.Context, name string) (string, error) {
	return "mock-password", nil
}

func (m *MockDatabaseManager) CreateSecret(ctx context.Context, name string, password string) error {
	return nil
}

func (m *MockDatabaseManager) DeleteDatabase(ctx context.Context, name string) error {
	return nil
}

func (m *MockDatabaseManager) DeleteDatabaseUser(ctx context.Context, name string) error {
	return nil
}

func (m *MockDatabaseManager) DeleteSecret(ctx context.Context, name string) error {
	return nil
}

// MockUnleashRepository mocks the unleash repository for testing
type MockUnleashRepository struct {
	instances map[string]*domainUnleash.Instance
}

func NewMockUnleashRepository() *MockUnleashRepository {
	return &MockUnleashRepository{
		instances: make(map[string]*domainUnleash.Instance),
	}
}

func (m *MockUnleashRepository) List(ctx context.Context, excludeChannelInstances bool) ([]*domainUnleash.Instance, error) {
	var result []*domainUnleash.Instance
	for _, instance := range m.instances {
		if excludeChannelInstances && instance.ReleaseChannelName != "" {
			continue
		}
		result = append(result, instance)
	}
	return result, nil
}

func (m *MockUnleashRepository) Get(ctx context.Context, name string) (*domainUnleash.Instance, error) {
	if instance, ok := m.instances[name]; ok {
		return instance, nil
	}
	return nil, errors.New("instance not found")
}

func (m *MockUnleashRepository) Create(ctx context.Context, config *domainUnleash.Config) error {
	m.instances[config.Name] = &domainUnleash.Instance{
		Name:               config.Name,
		Namespace:          "default",
		ReleaseChannelName: config.ReleaseChannelName,
		CustomVersion:      config.CustomVersion,
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
		EnableFederation:   config.EnableFederation,
		FederationNonce:    config.FederationNonce,
		AllowedTeams:       config.AllowedTeams,
		AllowedNamespaces:  config.AllowedNamespaces,
		AllowedClusters:    config.AllowedClusters,
	}
	return nil
}

func (m *MockUnleashRepository) Update(ctx context.Context, config *domainUnleash.Config) error {
	if _, ok := m.instances[config.Name]; !ok {
		return errors.New("instance not found")
	}
	existing := m.instances[config.Name]
	m.instances[config.Name] = &domainUnleash.Instance{
		Name:               config.Name,
		Namespace:          existing.Namespace,
		ReleaseChannelName: config.ReleaseChannelName,
		CustomVersion:      config.CustomVersion,
		Version:            existing.Version,
		CreatedAt:          existing.CreatedAt,
		EnableFederation:   config.EnableFederation,
		FederationNonce:    config.FederationNonce,
		AllowedTeams:       config.AllowedTeams,
		AllowedNamespaces:  config.AllowedNamespaces,
		AllowedClusters:    config.AllowedClusters,
	}
	return nil
}

func (m *MockUnleashRepository) Delete(ctx context.Context, name string) error {
	if _, ok := m.instances[name]; !ok {
		return errors.New("instance not found")
	}
	delete(m.instances, name)
	return nil
}

func (m *MockUnleashRepository) GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	instance, ok := m.instances[name]
	if !ok {
		return nil, errors.New("instance not found")
	}
	return &unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:              instance.Name,
			Namespace:         instance.Namespace,
			CreationTimestamp: metav1.NewTime(instance.CreatedAt),
		},
		Spec: unleashv1.UnleashSpec{
			CustomImage: instance.CustomVersion,
			ReleaseChannel: unleashv1.UnleashReleaseChannelConfig{
				Name: instance.ReleaseChannelName,
			},
		},
		Status: unleashv1.UnleashStatus{
			Version: instance.Version,
		},
	}, nil
}

func (m *MockUnleashRepository) ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error) {
	var result []unleashv1.Unleash
	for _, instance := range m.instances {
		if excludeChannelInstances && instance.ReleaseChannelName != "" {
			continue
		}
		result = append(result, unleashv1.Unleash{
			ObjectMeta: metav1.ObjectMeta{
				Name:              instance.Name,
				Namespace:         instance.Namespace,
				CreationTimestamp: metav1.NewTime(instance.CreatedAt),
			},
			Spec: unleashv1.UnleashSpec{
				CustomImage: instance.CustomVersion,
				ReleaseChannel: unleashv1.UnleashReleaseChannelConfig{
					Name: instance.ReleaseChannelName,
				},
			},
			Status: unleashv1.UnleashStatus{
				Version: instance.Version,
			},
		})
	}
	return result, nil
}

// setupUnleashTestHandler creates a test handler with default configuration.
// It's a convenience wrapper around setupUnleashTestHandlerWithConfig.
func setupUnleashTestHandler(repo *MockUnleashRepository, channelRepo *MockReleaseChannelRepository) (*UnleashHandler, *gin.Engine) {
	return setupUnleashTestHandlerWithConfig(repo, channelRepo, &config.Config{})
}

// setupUnleashTestHandlerWithConfig creates a test handler with custom configuration.
// Returns the handler and a configured gin router for testing HTTP endpoints.
func setupUnleashTestHandlerWithConfig(repo *MockUnleashRepository, channelRepo *MockReleaseChannelRepository, cfg *config.Config) (*UnleashHandler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	// Create a mock database manager
	dbManager := &MockDatabaseManager{}
	service := unleash.NewService(repo, dbManager, cfg, logger)

	handler := NewUnleashHandler(service, cfg, logger, channelRepo)
	return handler, router
}

// TestUpdateInstance_ReleaseChannelDowngradeProtection verifies that the handler
// prevents switching to a release channel with a lower major version
func TestUpdateInstance_ReleaseChannelDowngradeProtection(t *testing.T) {
	tests := []struct {
		name               string
		existingChannel    string
		newChannel         string
		existingVersion    string
		newVersion         string
		shouldFail         bool
		expectedStatusCode int
	}{
		{
			name:               "allow same major version",
			existingChannel:    "stable",
			newChannel:         "rapid",
			existingVersion:    "5.10.0",
			newVersion:         "5.11.0",
			shouldFail:         false,
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "allow upgrade to higher major version",
			existingChannel:    "stable",
			newChannel:         "next",
			existingVersion:    "5.10.0",
			newVersion:         "6.0.0",
			shouldFail:         false,
			expectedStatusCode: http.StatusOK,
		},
		{
			name:               "reject downgrade to lower major version",
			existingChannel:    "next",
			newChannel:         "stable",
			existingVersion:    "6.0.0",
			newVersion:         "5.11.0",
			shouldFail:         true,
			expectedStatusCode: http.StatusBadRequest,
		},
		{
			name:               "allow same channel (no-op)",
			existingChannel:    "stable",
			newChannel:         "stable",
			existingVersion:    "5.10.0",
			newVersion:         "5.10.0",
			shouldFail:         false,
			expectedStatusCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mock repository with the existing instance
			repo := NewMockUnleashRepository()
			repo.instances["test-instance"] = &domainUnleash.Instance{
				Name:               "test-instance",
				Namespace:          "default",
				ReleaseChannelName: tt.existingChannel,
				Version:            tt.existingVersion,
				CreatedAt:          time.Now(),
			}

			// Setup mock channel repository with test channels
			channelRepo := &MockReleaseChannelRepository{
				GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
					switch name {
					case "stable":
						return &releasechannel.Channel{
							Name:  "stable",
							Image: "quay.io/unleash/unleash-server:5.10.0",
						}, nil
					case "rapid":
						return &releasechannel.Channel{
							Name:  "rapid",
							Image: "quay.io/unleash/unleash-server:5.11.0",
						}, nil
					case "next":
						return &releasechannel.Channel{
							Name:  "next",
							Image: "quay.io/unleash/unleash-server:6.0.0",
						}, nil
					default:
						return nil, errors.New("channel not found")
					}
				},
			}

			handler, router := setupUnleashTestHandler(repo, channelRepo)
			router.PUT("/unleash/:name", handler.UpdateInstance)

			requestBody := map[string]interface{}{
				"release_channel_name": tt.newChannel,
				"log_level":            "info",
				"database_pool_max":    5,
			}
			body, _ := json.Marshal(requestBody)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("PUT", "/unleash/test-instance", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatusCode {
				t.Logf("Response body: %s", w.Body.String())
			}
			assert.Equal(t, tt.expectedStatusCode, w.Code, "unexpected status code")

			if tt.shouldFail {
				var response ErrorResponse
				err := json.Unmarshal(w.Body.Bytes(), &response)
				require.NoError(t, err)
				assert.Equal(t, "invalid_channel_switch", response.Error)
				assert.Contains(t, response.Message, "lower major version")
			}
		})
	}
}

func TestUpdateInstance_NewChannelAssignment(t *testing.T) {
	// Setup: instance with custom version, no channel
	repo := NewMockUnleashRepository()
	repo.instances["test-instance"] = &domainUnleash.Instance{
		Name:          "test-instance",
		Namespace:     "default",
		CustomVersion: "5.10.0",
		Version:       "5.10.0",
		CreatedAt:     time.Now(),
	}

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  "stable",
				Image: "quay.io/unleash/unleash-server:5.11.0",
			}, nil
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	requestBody := map[string]interface{}{
		"release_channel_name": "stable",
		"log_level":            "info",
		"database_pool_max":    5,
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/unleash/test-instance", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Should allow new channel assignment (no downgrade check)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateInstance_ChannelNotFound(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["test-instance"] = &domainUnleash.Instance{
		Name:               "test-instance",
		Namespace:          "default",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
	}

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return nil, errors.New("channel not found")
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	requestBody := map[string]interface{}{
		"release_channel_name": "nonexistent",
		"log_level":            "info",
		"database_pool_max":    5,
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/unleash/test-instance", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "invalid_channel_switch", response.Error)
}

func TestUpdateInstance_PreservesVersionSourceWhenNotSpecified(t *testing.T) {
	tests := []struct {
		name                   string
		existingCustomVersion  string
		existingReleaseChannel string
		expectedCustomVersion  string
		expectedReleaseChannel string
	}{
		{
			name:                   "preserves existing release channel",
			existingCustomVersion:  "",
			existingReleaseChannel: "stable",
			expectedCustomVersion:  "",
			expectedReleaseChannel: "stable",
		},
		{
			name:                   "preserves existing custom version",
			existingCustomVersion:  "5.9.0",
			existingReleaseChannel: "",
			expectedCustomVersion:  "5.9.0",
			expectedReleaseChannel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewMockUnleashRepository()
			repo.instances["test-instance"] = &domainUnleash.Instance{
				Name:               "test-instance",
				Namespace:          "default",
				CustomVersion:      tt.existingCustomVersion,
				ReleaseChannelName: tt.existingReleaseChannel,
				Version:            "5.10.0",
				CreatedAt:          time.Now(),
			}

			channelRepo := &MockReleaseChannelRepository{
				GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
					return &releasechannel.Channel{
						Name:  name,
						Image: "quay.io/unleash/unleash-server:5.10.0",
					}, nil
				},
			}

			handler, router := setupUnleashTestHandler(repo, channelRepo)
			router.PUT("/unleash/:name", handler.UpdateInstance)

			// Update without specifying version source - only change log level
			requestBody := map[string]interface{}{
				"log_level":         "debug",
				"database_pool_max": 5,
			}
			body, _ := json.Marshal(requestBody)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("PUT", "/unleash/test-instance", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

			// Verify the version source was preserved
			updated := repo.instances["test-instance"]
			require.NotNil(t, updated)
			assert.Equal(t, tt.expectedCustomVersion, updated.CustomVersion, "custom version should be preserved")
			assert.Equal(t, tt.expectedReleaseChannel, updated.ReleaseChannelName, "release channel should be preserved")
		})
	}
}

func TestCreateInstance_DefaultReleaseChannel(t *testing.T) {
	tests := []struct {
		name                   string
		configDefaultChannel   string
		requestCustomVersion   string
		requestReleaseChannel  string
		expectedReleaseChannel string
		expectedCustomVersion  string
	}{
		{
			name:                   "uses default channel when neither custom version nor channel specified",
			configDefaultChannel:   "stable",
			requestCustomVersion:   "",
			requestReleaseChannel:  "",
			expectedReleaseChannel: "stable",
			expectedCustomVersion:  "",
		},
		{
			name:                   "explicit custom version overrides default channel",
			configDefaultChannel:   "stable",
			requestCustomVersion:   "5.9.0",
			requestReleaseChannel:  "",
			expectedReleaseChannel: "",
			expectedCustomVersion:  "5.9.0",
		},
		{
			name:                   "explicit release channel overrides default channel",
			configDefaultChannel:   "stable",
			requestCustomVersion:   "",
			requestReleaseChannel:  "rapid",
			expectedReleaseChannel: "rapid",
			expectedCustomVersion:  "",
		},

		{
			name:                   "release channel takes precedence when both provided",
			configDefaultChannel:   "stable",
			requestCustomVersion:   "5.8.0",
			requestReleaseChannel:  "rapid",
			expectedReleaseChannel: "rapid",
			expectedCustomVersion:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := NewMockUnleashRepository()
			channelRepo := &MockReleaseChannelRepository{
				GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
					return &releasechannel.Channel{
						Name:  name,
						Image: "quay.io/unleash/unleash-server:5.10.0",
					}, nil
				},
			}

			cfg := &config.Config{
				Unleash: config.UnleashConfig{
					DefaultReleaseChannel: tt.configDefaultChannel,
				},
			}

			handler, router := setupUnleashTestHandlerWithConfig(repo, channelRepo, cfg)
			router.POST("/unleash", handler.CreateInstance)

			requestBody := map[string]interface{}{
				"name":              "test-instance",
				"log_level":         "info",
				"database_pool_max": 5,
			}

			if tt.requestCustomVersion != "" {
				requestBody["custom_version"] = tt.requestCustomVersion
			}

			if tt.requestReleaseChannel != "" {
				requestBody["release_channel_name"] = tt.requestReleaseChannel
			}

			body, _ := json.Marshal(requestBody)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusCreated, w.Code, "Response: %s", w.Body.String())

			// Verify the created instance has the expected values
			created := repo.instances["test-instance"]
			require.NotNil(t, created, "instance should be created")
			assert.Equal(t, tt.expectedReleaseChannel, created.ReleaseChannelName, "release channel mismatch")
			assert.Equal(t, tt.expectedCustomVersion, created.CustomVersion, "custom version mismatch")
		})
	}
}

func TestCreateInstance_ExplicitVersionsNotAffectedByDefault(t *testing.T) {
	// Ensure that when a user explicitly specifies a version or channel,
	// the default release channel configuration doesn't interfere
	repo := NewMockUnleashRepository()
	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  name,
				Image: "quay.io/unleash/unleash-server:5.10.0",
			}, nil
		},
	}

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			DefaultReleaseChannel: "stable",
		},
	}

	handler, router := setupUnleashTestHandlerWithConfig(repo, channelRepo, cfg)
	router.POST("/unleash", handler.CreateInstance)

	t.Run("custom version is respected", func(t *testing.T) {
		requestBody := map[string]interface{}{
			"name":              "custom-version-instance",
			"custom_version":    "4.20.0",
			"log_level":         "info",
			"database_pool_max": 5,
		}
		body, _ := json.Marshal(requestBody)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
		created := repo.instances["custom-version-instance"]
		assert.Equal(t, "4.20.0", created.CustomVersion)
		assert.Equal(t, "", created.ReleaseChannelName, "should not have release channel")
	})

	t.Run("explicit release channel is respected", func(t *testing.T) {
		requestBody := map[string]interface{}{
			"name":                 "explicit-channel-instance",
			"release_channel_name": "rapid",
			"log_level":            "info",
			"database_pool_max":    5,
		}
		body, _ := json.Marshal(requestBody)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
		created := repo.instances["explicit-channel-instance"]
		assert.Equal(t, "rapid", created.ReleaseChannelName)
		assert.Equal(t, "", created.CustomVersion, "should not have custom version")
	})
}

func TestCreateInstance_NoVersionSourceRejected(t *testing.T) {
	// When no default release channel is configured and user provides neither
	// custom version nor release channel, creation should be rejected
	repo := NewMockUnleashRepository()
	channelRepo := &MockReleaseChannelRepository{}

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			DefaultReleaseChannel: "", // No default configured
		},
	}

	handler, router := setupUnleashTestHandlerWithConfig(repo, channelRepo, cfg)
	router.POST("/unleash", handler.CreateInstance)

	requestBody := map[string]interface{}{
		"name":              "no-version-instance",
		"log_level":         "info",
		"database_pool_max": 5,
		// No custom-version and no release-channel-name
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "no_version_source", response.Error)
	assert.Contains(t, response.Message, "Must specify either custom-version or release-channel-name")

	// Instance should not have been created
	_, exists := repo.instances["no-version-instance"]
	assert.False(t, exists, "instance should not have been created")
}

func TestCreateInstance_OptionalFieldsUseDefaults(t *testing.T) {
	// When log-level and database-pool-max are not specified,
	// the instance should be created with default values
	repo := NewMockUnleashRepository()
	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  name,
				Image: "quay.io/unleash/unleash-server:5.10.0",
			}, nil
		},
	}

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			DefaultReleaseChannel: "stable",
		},
	}

	handler, router := setupUnleashTestHandlerWithConfig(repo, channelRepo, cfg)
	router.POST("/unleash", handler.CreateInstance)

	// Request with only name - no log-level, no database-pool-max
	requestBody := map[string]interface{}{
		"name": "minimal-instance",
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Should succeed - defaults are applied
	assert.Equal(t, http.StatusCreated, w.Code, "Response: %s", w.Body.String())

	created := repo.instances["minimal-instance"]
	require.NotNil(t, created, "instance should be created")
	assert.Equal(t, "stable", created.ReleaseChannelName, "should use default release channel")
}

func TestUpdateInstance_OptionalFieldsPreserveExisting(t *testing.T) {
	// When updating an instance without specifying log-level or database-pool-max,
	// the update should succeed (using defaults, not failing validation)
	repo := NewMockUnleashRepository()
	repo.instances["existing-instance"] = &domainUnleash.Instance{
		Name:               "existing-instance",
		Namespace:          "default",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
	}

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  name,
				Image: "quay.io/unleash/unleash-server:5.10.0",
			}, nil
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	// Update with minimal fields - no log-level, no database-pool-max
	requestBody := map[string]interface{}{
		"enable_federation": true,
		"allowed_teams":     "team1,team2",
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/unleash/existing-instance", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Should succeed - defaults are applied for missing fields
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())
}

func TestCreateInstance_FederationEnabledByDefault(t *testing.T) {
	// New instances should have federation enabled by default (new instances default)
	repo := NewMockUnleashRepository()
	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  name,
				Image: "quay.io/unleash/unleash-server:5.10.0",
			}, nil
		},
	}

	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			DefaultReleaseChannel: "stable",
		},
	}

	handler, router := setupUnleashTestHandlerWithConfig(repo, channelRepo, cfg)
	router.POST("/unleash", handler.CreateInstance)

	// Request without explicit enable-federation
	requestBody := map[string]interface{}{
		"name":          "new-instance",
		"allowed_teams": "myteam",
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "Response: %s", w.Body.String())

	created := repo.instances["new-instance"]
	require.NotNil(t, created, "instance should be created")
	assert.True(t, created.EnableFederation, "federation should be enabled by default")
	assert.Equal(t, "myteam", created.AllowedTeams, "allowed teams should be set")
}

func TestUpdateInstance_PreservesFederationSettings(t *testing.T) {
	// When updating an instance, federation settings should be preserved if not specified
	repo := NewMockUnleashRepository()
	repo.instances["fed-instance"] = &domainUnleash.Instance{
		Name:               "fed-instance",
		Namespace:          "default",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
		EnableFederation:   true,
		FederationNonce:    "abc12345",
		AllowedTeams:       "team-a,team-b",
		AllowedNamespaces:  "team-a,team-b",
		AllowedClusters:    "dev-gcp,prod-gcp",
	}

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:  name,
				Image: "quay.io/unleash/unleash-server:5.10.0",
			}, nil
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	// Update with only allowed-teams - should preserve nonce and other federation settings
	requestBody := map[string]interface{}{
		"allowed_teams": "team-a,team-b,team-c",
	}
	body, _ := json.Marshal(requestBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/unleash/fed-instance", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated, "instance should exist")
	assert.True(t, updated.EnableFederation, "federation should remain enabled")
	assert.Equal(t, "abc12345", updated.FederationNonce, "federation nonce should be preserved")
	assert.Equal(t, "team-a,team-b,team-c", updated.AllowedTeams, "allowed teams should be updated")
	// Note: MergeTeamsAndNamespaces merges teams and namespaces into both fields
	assert.Equal(t, "team-a,team-b,team-c", updated.AllowedNamespaces, "allowed namespaces are merged with teams")
	assert.Equal(t, "dev-gcp,prod-gcp", updated.AllowedClusters, "allowed clusters should be preserved")
}
