package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MockReleaseChannelRepository is a mock implementation of the release channel repository for testing
type MockReleaseChannelRepository struct {
	ListFunc func(ctx context.Context) ([]*releasechannel.Channel, error)
	GetFunc  func(ctx context.Context, name string) (*releasechannel.Channel, error)
}

func (m *MockReleaseChannelRepository) List(ctx context.Context) ([]*releasechannel.Channel, error) {
	if m.ListFunc != nil {
		return m.ListFunc(ctx)
	}
	return nil, nil
}

func (m *MockReleaseChannelRepository) Get(ctx context.Context, name string) (*releasechannel.Channel, error) {
	if m.GetFunc != nil {
		return m.GetFunc(ctx, name)
	}
	return nil, errors.New("not found")
}

func setupTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestListChannels_Success(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		ListFunc: func(ctx context.Context) ([]*releasechannel.Channel, error) {
			return []*releasechannel.Channel{
				{
					Name:      "stable",
					Image:     "quay.io/unleash/unleash-server:5.11.0",
					CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
					Spec: releasechannel.ChannelSpec{
						MaxParallel:   2,
						CanaryEnabled: false,
					},
					Status: releasechannel.ChannelStatus{
						Phase:               "Completed",
						Instances:           10,
						InstancesUpToDate:   10,
						Progress:            100,
						Completed:           true,
						LastReconcileTime:   metav1.NewTime(time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)),
						LastImageChangeTime: metav1.NewTime(time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)),
					},
				},
				{
					Name:      "rapid",
					Image:     "quay.io/unleash/unleash-server:5.12.0-beta.1",
					CreatedAt: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
					Spec: releasechannel.ChannelSpec{
						MaxParallel:   1,
						CanaryEnabled: true,
					},
					Status: releasechannel.ChannelStatus{
						Phase:             "Rolling",
						Instances:         5,
						InstancesUpToDate: 3,
						Progress:          60,
						Completed:         false,
						LastReconcileTime: metav1.NewTime(time.Date(2024, 3, 20, 14, 15, 0, 0, time.UTC)),
					},
				},
			}, nil
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels", handler.ListChannels)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []ReleaseChannelResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, 2, len(response))

	// Verify first channel - only configuration fields
	assert.Equal(t, "stable", response[0].Name)
	assert.Equal(t, "quay.io/unleash/unleash-server:5.11.0", response[0].Image)
	assert.Equal(t, "2024-01-01T00:00:00Z", response[0].CreatedAt)

	// Verify legacy fields (backwards compatibility)
	assert.Equal(t, response[0].Image, response[0].Version, "Version should equal Image for backwards compatibility")
	// CurrentVersion comes from status.version which is typically empty until set by the controller
	assert.Equal(t, "", response[0].CurrentVersion, "CurrentVersion should be empty when status.version is not set")
	// LastUpdated comes from status.lastImageChangeTime
	assert.Equal(t, "2024-03-15T10:30:00Z", response[0].LastUpdated, "LastUpdated should be populated from lastImageChangeTime")

	// Verify second channel
	assert.Equal(t, "rapid", response[1].Name)
	assert.Equal(t, "quay.io/unleash/unleash-server:5.12.0-beta.1", response[1].Image)
	assert.Equal(t, "2024-02-01T00:00:00Z", response[1].CreatedAt)
}

func TestListChannels_EmptyList(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		ListFunc: func(ctx context.Context) ([]*releasechannel.Channel, error) {
			return []*releasechannel.Channel{}, nil
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels", handler.ListChannels)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []ReleaseChannelResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, 0, len(response))
}

func TestListChannels_ServiceError(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		ListFunc: func(ctx context.Context) ([]*releasechannel.Channel, error) {
			return nil, errors.New("kubernetes API error")
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels", handler.ListChannels)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "failed_to_list", response.Error)
}

func TestGetChannel_Success(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			assert.Equal(t, "stable", name)
			return &releasechannel.Channel{
				Name:      "stable",
				Image:     "quay.io/unleash/unleash-server:5.11.0",
				CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				Spec: releasechannel.ChannelSpec{
					MaxParallel:   2,
					CanaryEnabled: false,
				},
				Status: releasechannel.ChannelStatus{
					Phase:               "Completed",
					Instances:           10,
					InstancesUpToDate:   10,
					Progress:            100,
					Completed:           true,
					LastImageChangeTime: metav1.NewTime(time.Date(2024, 3, 15, 10, 30, 0, 0, time.UTC)),
				},
			}, nil
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels/:name", handler.GetChannel)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels/stable", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response ReleaseChannelResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "stable", response.Name)
	assert.Equal(t, "quay.io/unleash/unleash-server:5.11.0", response.Image)
	assert.Equal(t, "2024-01-01T00:00:00Z", response.CreatedAt)
	// Verify status fields
	assert.Equal(t, "", response.CurrentVersion)
	assert.Equal(t, "2024-03-15T10:30:00Z", response.LastUpdated)
	// Legacy fields
	assert.Equal(t, response.Image, response.Version)
}

func TestGetChannel_WithoutLastReconciled(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return &releasechannel.Channel{
				Name:      "testing",
				Image:     "quay.io/unleash/unleash-server:5.13.0-rc.1",
				CreatedAt: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
				Spec: releasechannel.ChannelSpec{
					MaxParallel:   4,
					CanaryEnabled: true,
				},
				Status: releasechannel.ChannelStatus{
					Phase:               "Idle",
					Instances:           0,
					Progress:            0,
					LastImageChangeTime: metav1.Time{}, // Zero time
				},
			}, nil
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels/:name", handler.GetChannel)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels/testing", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response ReleaseChannelResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "testing", response.Name)
	assert.Equal(t, "quay.io/unleash/unleash-server:5.13.0-rc.1", response.Image)
	assert.Empty(t, response.LastUpdated, "LastUpdated should be empty when lastImageChangeTime is zero")
}

func TestGetChannel_NotFound(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return nil, errors.New("not found")
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels/:name", handler.GetChannel)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels/nonexistent", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "not_found", response.Error)
	assert.Equal(t, "Release channel not found", response.Message)
	assert.Equal(t, "nonexistent", response.Details["channel"])
}

func TestGetChannel_ServiceError(t *testing.T) {
	mockRepo := &MockReleaseChannelRepository{
		GetFunc: func(ctx context.Context, name string) (*releasechannel.Channel, error) {
			return nil, errors.New("kubernetes API error")
		},
	}

	router := setupTestRouter()
	logger := logrus.New()
	handler := NewReleaseChannelHandler(mockRepo, logger)
	router.GET("/v1/channels/:name", handler.GetChannel)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/channels/stable", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var response ErrorResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "not_found", response.Error)
}
