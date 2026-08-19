package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MockDatabaseManager implements the DatabaseManager interface for testing
type MockDatabaseManager struct{}

func (m *MockDatabaseManager) CreateDatabase(ctx context.Context, name string) (bool, error) {
	return true, nil
}

func (m *MockDatabaseManager) CreateDatabaseUser(ctx context.Context, name string) (string, bool, error) {
	return "mock-password", true, nil
}

func (m *MockDatabaseManager) CreateSecret(ctx context.Context, name string, password string) (bool, error) {
	return true, nil
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

	// beforeUpdate runs at the top of Update, before its precondition is
	// checked. It is how a test models another writer — a reconciler, a second
	// client — landing between the handler's read and the repository's write,
	// which is the only place the lost-update race can happen.
	beforeUpdate func()
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
	// Mirror the real repository, which wraps a Kubernetes NotFound. Callers
	// distinguish "does not exist" from "could not tell", so a plain error
	// here would exercise a path production never takes.
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "unleash.nais.io", Resource: "unleashes"}, name)
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

// nextResourceVersion models the API server bumping the version on every write,
// so a caller holding the pre-write version is detectably stale.
func nextResourceVersion(current string) string {
	n, err := strconv.Atoi(current)
	if err != nil {
		// Instances in tests that do not care about concurrency leave it empty;
		// keep them at "" so their writes stay unconditional.
		return current
	}
	return strconv.Itoa(n + 1)
}

func (m *MockUnleashRepository) Update(ctx context.Context, config *domainUnleash.Config, opts domainUnleash.UpdateOptions) error {
	if m.beforeUpdate != nil {
		m.beforeUpdate()
	}
	if _, ok := m.instances[config.Name]; !ok {
		return errors.New("instance not found")
	}
	existing := m.instances[config.Name]

	// Mirror the API server: a supplied resourceVersion makes the write
	// conditional, and a stale one is rejected rather than applied.
	if opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != existing.ResourceVersion {
		return apierrors.NewConflict(
			schema.GroupResource{Group: "unleash.nais.io", Resource: "unleashes"},
			config.Name,
			errors.New("the object has been modified; please apply your changes to the latest version and try again"),
		)
	}

	m.instances[config.Name] = &domainUnleash.Instance{
		Name:               config.Name,
		Namespace:          existing.Namespace,
		ResourceVersion:    nextResourceVersion(existing.ResourceVersion),
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
				assert.Contains(t, response.Message, "downgrade")
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
	assert.Contains(t, response.Message, "Must specify")

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

// federatedInstance returns an instance whose allowed teams and namespaces are
// in sync, matching what MergeTeamsAndNamespaces leaves behind in production.
func federatedInstance(name, teams string) *domainUnleash.Instance {
	return &domainUnleash.Instance{
		Name:               name,
		Namespace:          "default",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
		EnableFederation:   true,
		FederationNonce:    "abc12345",
		AllowedTeams:       teams,
		AllowedNamespaces:  teams,
		AllowedClusters:    "dev-gcp,prod-gcp",
	}
}

func updateInstanceRequest(t *testing.T, repo *MockUnleashRepository, name string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{Name: name, Image: "quay.io/unleash/unleash-server:5.10.0"}, nil
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/unleash/"+name, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

// Revoking a team must actually remove it. Before the fix the update handler
// restored allowed_namespaces from the CRD and MergeTeamsAndNamespaces unioned
// it back in, so the revoked team reappeared and the CRD was written unchanged.
func TestUpdateInstance_RevokesTeamAccess(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["fed-instance"] = federatedInstance("fed-instance", "team-a,team-b")

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"allowed_teams": "team-a",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated)
	assert.Equal(t, "team-a", updated.AllowedTeams, "team-b must be removed from allowed teams")
	assert.Equal(t, "team-a", updated.AllowedNamespaces, "team-b must be removed from allowed namespaces")
	assert.Equal(t, "abc12345", updated.FederationNonce, "unrelated federation settings stay put")
	assert.Equal(t, "dev-gcp,prod-gcp", updated.AllowedClusters)
}

// Revoking the last team supplies an empty list. That must be distinguishable
// from omitting the field, otherwise the list can never be emptied.
func TestUpdateInstance_RevokesLastTeamAccess(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["fed-instance"] = federatedInstance("fed-instance", "team-a")

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"allowed_teams": "",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated)
	assert.Empty(t, updated.AllowedTeams, "an explicitly empty list must empty the allowed teams")
	assert.Empty(t, updated.AllowedNamespaces, "an explicitly empty list must empty the allowed namespaces")
}

// Omitting the field entirely is not a statement about access and must preserve
// whatever is on the instance — an unrelated update must never revoke.
func TestUpdateInstance_PreservesTeamsWhenNotSupplied(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["fed-instance"] = federatedInstance("fed-instance", "team-a,team-b")

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"log_level": "debug",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated)
	assert.Equal(t, "team-a,team-b", updated.AllowedTeams, "an unrelated update must not revoke access")
	assert.Equal(t, "team-a,team-b", updated.AllowedNamespaces)
}

// A legacy client that only knows the deprecated namespaces field must still be
// able to state the desired set, and it must reach the authoritative field.
func TestUpdateInstance_DeprecatedNamespacesFieldStillRevokes(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["fed-instance"] = federatedInstance("fed-instance", "team-a,team-b")

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"allowed_namespaces": "team-a",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated)
	assert.Equal(t, "team-a", updated.AllowedTeams)
	assert.Equal(t, "team-a", updated.AllowedNamespaces)
}

// Instances predating the fix may carry differing lists. Preserving must take
// the union so an unrelated update never silently drops an entry.
func TestUpdateInstance_PreservesUnionOfDivergedLists(t *testing.T) {
	repo := NewMockUnleashRepository()
	instance := federatedInstance("fed-instance", "team-a")
	instance.AllowedNamespaces = "team-b"
	repo.instances["fed-instance"] = instance

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"log_level": "debug",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	updated := repo.instances["fed-instance"]
	require.NotNil(t, updated)
	assert.Equal(t, "team-a,team-b", updated.AllowedTeams, "preserving must not drop either side")
	assert.Equal(t, "team-a,team-b", updated.AllowedNamespaces)
}

// The handler reads the instance, merges the request body onto what it read,
// and only then writes. A write landing in that window used to be overwritten
// wholesale: the migration reconciler moving an instance onto a release channel
// was reverted to the custom version the caller had read, with a 200 back to the
// caller and no trace that anything was lost.
func TestUpdateInstance_ConflictingWriteIsRefused(t *testing.T) {
	repo := NewMockUnleashRepository()
	pre := federatedInstance("fed-instance", "team-a")
	pre.ResourceVersion = "1"
	pre.ReleaseChannelName = ""
	pre.CustomVersion = "v5.1.2"
	repo.instances["fed-instance"] = pre

	// The migration reconciler writes through the repository directly, so it is
	// not serialized by the service's per-instance lock and can land here.
	repo.beforeUpdate = func() {
		migrated := federatedInstance("fed-instance", "team-a")
		migrated.ResourceVersion = "2"
		migrated.ReleaseChannelName = "stable-v6"
		repo.instances["fed-instance"] = migrated
	}

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"log_level": "debug",
	})
	assert.Equal(t, http.StatusConflict, w.Code, "Response: %s", w.Body.String())

	var body ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "conflict", body.Error)

	current := repo.instances["fed-instance"]
	require.NotNil(t, current)
	assert.Equal(t, "stable-v6", current.ReleaseChannelName, "the migration must survive the losing update")
	assert.Empty(t, current.CustomVersion, "the stale custom version must not be written back")
}

// The precondition must only reject genuine conflicts: an update on an instance
// nobody else touched still has to go through.
func TestUpdateInstance_UndisturbedWriteSucceeds(t *testing.T) {
	repo := NewMockUnleashRepository()
	instance := federatedInstance("fed-instance", "team-a")
	instance.ResourceVersion = "1"
	repo.instances["fed-instance"] = instance

	w := updateInstanceRequest(t, repo, "fed-instance", map[string]any{
		"log_level": "debug",
	})
	assert.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())
	assert.Equal(t, "2", repo.instances["fed-instance"].ResourceVersion, "the write must have landed")
}

// Create must refuse an instance that already exists. Without this, a duplicate
// POST runs the whole provisioning path against live resources, and any failure
// along the way triggers a rollback that tears them down.
func TestCreateInstance_RefusesExistingInstance(t *testing.T) {
	repo := NewMockUnleashRepository()
	repo.instances["team-a"] = federatedInstance("team-a", "team-a")

	channelRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{Name: name, Image: "quay.io/unleash/unleash-server:5.10.0"}, nil
		},
	}
	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.POST("/unleash", handler.CreateInstance)

	body, err := json.Marshal(map[string]any{"name": "team-a", "release_channel_name": "stable"})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/unleash", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code, "Response: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "already_exists")
}
