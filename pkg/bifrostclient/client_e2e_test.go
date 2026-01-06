package bifrostclient_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/api/generated"
	v1 "github.com/nais/bifrost/pkg/api/http/v1"
	"github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/bifrostclient"
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

func (m *MockUnleashRepository) Create(ctx context.Context, cfg *domainUnleash.Config) error {
	m.instances[cfg.Name] = &domainUnleash.Instance{
		Name:               cfg.Name,
		Namespace:          "default",
		ReleaseChannelName: cfg.ReleaseChannelName,
		CustomVersion:      cfg.CustomVersion,
		Version:            "5.10.0",
		CreatedAt:          time.Now(),
		EnableFederation:   cfg.EnableFederation,
		FederationNonce:    cfg.FederationNonce,
		AllowedTeams:       cfg.AllowedTeams,
		AllowedNamespaces:  cfg.AllowedNamespaces,
		AllowedClusters:    cfg.AllowedClusters,
	}
	return nil
}

func (m *MockUnleashRepository) Update(ctx context.Context, cfg *domainUnleash.Config) error {
	if _, ok := m.instances[cfg.Name]; !ok {
		return errors.New("instance not found")
	}
	existing := m.instances[cfg.Name]
	m.instances[cfg.Name] = &domainUnleash.Instance{
		Name:               cfg.Name,
		Namespace:          existing.Namespace,
		ReleaseChannelName: cfg.ReleaseChannelName,
		CustomVersion:      cfg.CustomVersion,
		Version:            existing.Version,
		CreatedAt:          existing.CreatedAt,
		EnableFederation:   cfg.EnableFederation,
		FederationNonce:    cfg.FederationNonce,
		AllowedTeams:       cfg.AllowedTeams,
		AllowedNamespaces:  cfg.AllowedNamespaces,
		AllowedClusters:    cfg.AllowedClusters,
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
		TypeMeta: metav1.TypeMeta{
			APIVersion: "unleash.nais.io/v1",
			Kind:       "Unleash",
		},
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
			TypeMeta: metav1.TypeMeta{
				APIVersion: "unleash.nais.io/v1",
				Kind:       "Unleash",
			},
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

// MockReleaseChannelRepository is a mock implementation of the release channel repository
type MockReleaseChannelRepository struct {
	channels map[string]*releasechannel.Channel
}

func NewMockReleaseChannelRepository() *MockReleaseChannelRepository {
	return &MockReleaseChannelRepository{
		channels: make(map[string]*releasechannel.Channel),
	}
}

func (m *MockReleaseChannelRepository) List(ctx context.Context) ([]*releasechannel.Channel, error) {
	var result []*releasechannel.Channel
	for _, channel := range m.channels {
		result = append(result, channel)
	}
	return result, nil
}

func (m *MockReleaseChannelRepository) Get(ctx context.Context, name string) (*releasechannel.Channel, error) {
	if channel, ok := m.channels[name]; ok {
		return channel, nil
	}
	return nil, errors.New("channel not found")
}

// setupTestServer creates a test HTTP server with the full API stack
func setupTestServer(
	unleashRepo domainUnleash.Repository,
	channelRepo releasechannel.Repository,
	dbManager unleash.DatabaseManager,
	cfg *config.Config,
) *httptest.Server {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	// Create the service with mocked dependencies
	service := unleash.NewService(unleashRepo, dbManager, cfg, logger)

	// Create the OpenAPI handler (same as production)
	openAPIHandler := v1.NewOpenAPIHandler(service, cfg, logger, channelRepo)

	// Register routes on the root router (paths already include /v1 prefix)
	generated.RegisterHandlers(router, openAPIHandler)

	return httptest.NewServer(router)
}

// TestClient_ListInstances_E2E tests that the client can list instances and correctly parse the response
func TestClient_ListInstances_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["test-instance"] = &domainUnleash.Instance{
		Name:               "test-instance",
		Namespace:          "unleash",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.ListInstancesWithResponse(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode())
	require.NotNil(t, resp.JSON200)
	require.Len(t, *resp.JSON200, 1)

	instance := (*resp.JSON200)[0]
	assert.Equal(t, "test-instance", *instance.Metadata.Name)
	assert.Equal(t, "unleash", *instance.Metadata.Namespace)
}

// TestClient_GetInstance_E2E tests that the client can get an instance with releaseChannel as object
func TestClient_GetInstance_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["my-unleash"] = &domainUnleash.Instance{
		Name:               "my-unleash",
		Namespace:          "unleash",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetInstanceWithResponse(context.Background(), "my-unleash")
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode())
	require.NotNil(t, resp.JSON200)

	// This is the critical test - releaseChannel should be an object with a name field
	require.NotNil(t, resp.JSON200.Spec)
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel, "ReleaseChannel should be present in spec")
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel.Name, "ReleaseChannel.Name should be present")
	assert.Equal(t, "stable", *resp.JSON200.Spec.ReleaseChannel.Name)

	// Also verify other fields
	assert.Equal(t, "my-unleash", *resp.JSON200.Metadata.Name)
	assert.Equal(t, "5.10.0", *resp.JSON200.Status.Version)
}

// TestClient_GetInstance_NotFound_E2E tests 404 handling
func TestClient_GetInstance_NotFound_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetInstanceWithResponse(context.Background(), "nonexistent")
	require.NoError(t, err)

	assert.Equal(t, 404, resp.StatusCode())
	require.NotNil(t, resp.JSON404)
	assert.Equal(t, "not_found", resp.JSON404.Error)
}

// TestClient_CreateInstance_E2E tests creating an instance with release channel
func TestClient_CreateInstance_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.channels["stable"] = &releasechannel.Channel{
		Name:      "stable",
		Image:     "quay.io/unleash/unleash-server:5.10.0",
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Status: releasechannel.ChannelStatus{
			Version: "5.10.0",
		},
	}
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{
		Unleash: config.UnleashConfig{
			DefaultReleaseChannel: "stable",
		},
	}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	name := "new-instance"
	releaseChannel := "stable"
	resp, err := client.CreateInstanceWithResponse(
		context.Background(),
		bifrostclient.CreateInstanceJSONRequestBody{
			Name:               &name,
			ReleaseChannelName: &releaseChannel,
		},
	)
	require.NoError(t, err)

	assert.Equal(t, 201, resp.StatusCode())
	require.NotNil(t, resp.JSON201)
	assert.Equal(t, "new-instance", *resp.JSON201.Metadata.Name)

	// Verify releaseChannel is correctly structured in response
	require.NotNil(t, resp.JSON201.Spec.ReleaseChannel)
	require.NotNil(t, resp.JSON201.Spec.ReleaseChannel.Name)
	assert.Equal(t, "stable", *resp.JSON201.Spec.ReleaseChannel.Name)
}

// TestClient_UpdateInstance_E2E tests updating an instance
func TestClient_UpdateInstance_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["existing-instance"] = &domainUnleash.Instance{
		Name:               "existing-instance",
		Namespace:          "unleash",
		ReleaseChannelName: "stable",
		Version:            "5.10.0",
		CreatedAt:          time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		EnableFederation:   true,
	}

	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.channels["stable"] = &releasechannel.Channel{
		Name:  "stable",
		Image: "quay.io/unleash/unleash-server:5.10.0",
		Status: releasechannel.ChannelStatus{
			Version: "5.10.0",
		},
	}
	channelRepo.channels["rapid"] = &releasechannel.Channel{
		Name:  "rapid",
		Image: "quay.io/unleash/unleash-server:5.11.0",
		Status: releasechannel.ChannelStatus{
			Version: "5.11.0",
		},
	}

	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	// Update to a new release channel
	newChannel := "rapid"
	resp, err := client.UpdateInstanceWithResponse(
		context.Background(),
		"existing-instance",
		bifrostclient.UpdateInstanceJSONRequestBody{
			ReleaseChannelName: &newChannel,
		},
	)
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode())
	require.NotNil(t, resp.JSON200)

	// Verify the update was applied and response structure is correct
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel)
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel.Name)
	assert.Equal(t, "rapid", *resp.JSON200.Spec.ReleaseChannel.Name)
}

// TestClient_DeleteInstance_E2E tests deleting an instance
func TestClient_DeleteInstance_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["to-delete"] = &domainUnleash.Instance{
		Name:      "to-delete",
		Namespace: "unleash",
		Version:   "5.10.0",
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.DeleteInstanceWithResponse(context.Background(), "to-delete")
	require.NoError(t, err)

	assert.Equal(t, 204, resp.StatusCode())

	// Verify instance is deleted
	getResp, err := client.GetInstanceWithResponse(context.Background(), "to-delete")
	require.NoError(t, err)
	assert.Equal(t, 404, getResp.StatusCode())
}

// TestClient_ListChannels_E2E tests listing release channels
func TestClient_ListChannels_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.channels["stable"] = &releasechannel.Channel{
		Name:      "stable",
		Image:     "quay.io/unleash/unleash-server:5.10.0",
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Status: releasechannel.ChannelStatus{
			Version:           "5.10.0",
			Phase:             "Completed",
			Instances:         5,
			InstancesUpToDate: 5,
			Progress:          100,
			Completed:         true,
		},
	}
	channelRepo.channels["rapid"] = &releasechannel.Channel{
		Name:      "rapid",
		Image:     "quay.io/unleash/unleash-server:5.11.0-beta.1",
		CreatedAt: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
		Status: releasechannel.ChannelStatus{
			Version:           "5.11.0-beta.1",
			Phase:             "Rolling",
			Instances:         3,
			InstancesUpToDate: 2,
			Progress:          66,
			Completed:         false,
		},
	}

	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.ListChannelsWithResponse(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode())
	require.NotNil(t, resp.JSON200)
	assert.Len(t, *resp.JSON200, 2)
}

// TestClient_GetChannel_E2E tests getting a specific release channel
func TestClient_GetChannel_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	channelRepo.channels["stable"] = &releasechannel.Channel{
		Name:      "stable",
		Image:     "quay.io/unleash/unleash-server:5.10.0",
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Status: releasechannel.ChannelStatus{
			Version:   "5.10.0",
			Phase:     "Completed",
			Completed: true,
		},
	}

	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetChannelWithResponse(context.Background(), "stable")
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode())
	require.NotNil(t, resp.JSON200)
	assert.Equal(t, "stable", resp.JSON200.Name)
	assert.Equal(t, "quay.io/unleash/unleash-server:5.10.0", resp.JSON200.Image)
	assert.Equal(t, "5.10.0", resp.JSON200.CurrentVersion)
}

// TestClient_GetChannel_NotFound_E2E tests 404 handling for release channels
func TestClient_GetChannel_NotFound_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetChannelWithResponse(context.Background(), "nonexistent")
	require.NoError(t, err)

	assert.Equal(t, 404, resp.StatusCode())
	require.NotNil(t, resp.JSON404)
	assert.Equal(t, "not_found", resp.JSON404.Error)
}

// TestClient_CreateInstance_NoVersionSource_E2E tests validation error when no version source is specified
func TestClient_CreateInstance_NoVersionSource_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	// No default release channel configured
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	name := "no-version-instance"
	resp, err := client.CreateInstanceWithResponse(
		context.Background(),
		bifrostclient.CreateInstanceJSONRequestBody{
			Name: &name,
			// No release_channel_name specified
		},
	)
	require.NoError(t, err)

	assert.Equal(t, 400, resp.StatusCode())
	require.NotNil(t, resp.JSON400)
	assert.Equal(t, "no_version_source", resp.JSON400.Error)
}

// TestClient_ReleaseChannelStructure_E2E is a regression test ensuring releaseChannel
// is correctly serialized as an object with a name field, not as a plain string
func TestClient_ReleaseChannelStructure_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["test"] = &domainUnleash.Instance{
		Name:               "test",
		Namespace:          "unleash",
		ReleaseChannelName: "my-channel",
		Version:            "5.10.0",
		CreatedAt:          time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetInstanceWithResponse(context.Background(), "test")
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode())

	// This is the regression test for the bug where releaseChannel was a string
	// instead of an object with name field
	require.NotNil(t, resp.JSON200.Spec, "Spec should be present")
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel, "ReleaseChannel should be an object, not nil")
	require.NotNil(t, resp.JSON200.Spec.ReleaseChannel.Name, "ReleaseChannel.Name should be present")
	assert.Equal(t, "my-channel", *resp.JSON200.Spec.ReleaseChannel.Name,
		"ReleaseChannel.Name should contain the channel name")
}

// TestClient_CustomImage_E2E tests that customImage is correctly handled
func TestClient_CustomImage_E2E(t *testing.T) {
	unleashRepo := NewMockUnleashRepository()
	unleashRepo.instances["custom"] = &domainUnleash.Instance{
		Name:          "custom",
		Namespace:     "unleash",
		CustomVersion: "my-registry/unleash:custom-tag",
		Version:       "custom",
		CreatedAt:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	channelRepo := NewMockReleaseChannelRepository()
	dbManager := &MockDatabaseManager{}
	cfg := &config.Config{}

	server := setupTestServer(unleashRepo, channelRepo, dbManager, cfg)
	defer server.Close()

	client, err := bifrostclient.NewClientWithResponses(server.URL)
	require.NoError(t, err)

	resp, err := client.GetInstanceWithResponse(context.Background(), "custom")
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode())

	require.NotNil(t, resp.JSON200.Spec)
	require.NotNil(t, resp.JSON200.Spec.CustomImage)
	assert.Equal(t, "my-registry/unleash:custom-tag", *resp.JSON200.Spec.CustomImage)

	// ReleaseChannel should be empty/nil when using custom image
	if resp.JSON200.Spec.ReleaseChannel != nil && resp.JSON200.Spec.ReleaseChannel.Name != nil {
		assert.Empty(t, *resp.JSON200.Spec.ReleaseChannel.Name)
	}
}
