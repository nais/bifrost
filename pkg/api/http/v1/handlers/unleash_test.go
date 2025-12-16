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
	"github.com/nais/bifrost/pkg/infrastructure/cloudsql"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func setupUnleashTestHandler(repo *MockUnleashRepository, channelRepo *MockReleaseChannelRepository) (*UnleashHandler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	// Create a real cloudsql.Manager with nil dependencies (won't be used in these tests)
	dbManager := cloudsql.NewManager(nil, nil, nil, cfg, logger)
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
							Name:    "stable",
							Version: "5.10.0",
							Status:  releasechannel.ChannelStatus{CurrentVersion: "5.10.0"},
						}, nil
					case "rapid":
						return &releasechannel.Channel{
							Name:    "rapid",
							Version: "5.11.0",
							Status:  releasechannel.ChannelStatus{CurrentVersion: "5.11.0"},
						}, nil
					case "next":
						return &releasechannel.Channel{
							Name:    "next",
							Version: "6.0.0",
							Status:  releasechannel.ChannelStatus{CurrentVersion: "6.0.0"},
						}, nil
					default:
						return nil, errors.New("channel not found")
					}
				},
			}

			handler, router := setupUnleashTestHandler(repo, channelRepo)
			router.PUT("/unleash/:name", handler.UpdateInstance)

			requestBody := map[string]interface{}{
				"release-channel-name": tt.newChannel,
				"log-level":            "info",
				"database-pool-max":    5,
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
				Name:    "stable",
				Version: "5.11.0",
			}, nil
		},
	}

	handler, router := setupUnleashTestHandler(repo, channelRepo)
	router.PUT("/unleash/:name", handler.UpdateInstance)

	requestBody := map[string]interface{}{
		"release-channel-name": "stable",
		"log-level":            "info",
		"database-pool-max":    5,
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
		"release-channel-name": "nonexistent",
		"log-level":            "info",
		"database-pool-max":    5,
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
