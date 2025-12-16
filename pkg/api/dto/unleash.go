package dto

import (
	"github.com/nais/bifrost/pkg/domain/unleash"
)

// UnleashConfigRequest represents the JSON request body for creating/updating an Unleash instance
type UnleashConfigRequest struct {
	Name                      string `json:"name,omitempty"`
	CustomVersion             string `json:"custom_version,omitempty"`
	ReleaseChannelName        string `json:"release_channel_name,omitempty"`
	EnableFederation          bool   `json:"enable_federation,omitempty"`
	FederationNonce           string `json:"-"` // Internal use only, not exposed in API
	AllowedTeams              string `json:"allowed_teams,omitempty"`
	AllowedNamespaces         string `json:"allowed_namespaces,omitempty"`
	AllowedClusters           string `json:"allowed_clusters,omitempty"`
	LogLevel                  string `json:"log_level,omitempty"`
	DatabasePoolMax           int    `json:"database_pool_max,omitempty"`
	DatabasePoolIdleTimeoutMs int    `json:"database_pool_idle_timeout_ms,omitempty"`
}

// UnleashInstanceResponse represents the JSON response for an Unleash instance
type UnleashInstanceResponse struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	CreatedAt   string `json:"created_at"`
	Age         string `json:"age"`
	Version     string `json:"version"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	APIUrl      string `json:"api_url"`
	WebUrl      string `json:"web_url"`

	// Version source
	VersionSource      string `json:"version_source,omitempty"`
	CustomVersion      string `json:"custom_version,omitempty"`
	ReleaseChannelName string `json:"release_channel_name,omitempty"`

	// Read-only status
	ResolvedImage         string `json:"resolved_image,omitempty"`
	ChannelNameFromStatus string `json:"channel_name_from_status,omitempty"`
}

// UnleashListResponse represents a list of Unleash instances
type UnleashListResponse struct {
	Instances []*UnleashInstanceResponse `json:"instances"`
	Count     int                        `json:"count"`
}

// ToConfigBuilder converts a request DTO to a domain ConfigBuilder
func (r *UnleashConfigRequest) ToConfigBuilder() *unleash.ConfigBuilder {
	builder := unleash.NewConfigBuilder().
		WithName(r.Name)

	if r.LogLevel != "" {
		builder.WithLogLevel(r.LogLevel)
	}

	if r.DatabasePoolMax > 0 || r.DatabasePoolIdleTimeoutMs > 0 {
		builder.WithDatabasePool(r.DatabasePoolMax, r.DatabasePoolIdleTimeoutMs)
	}

	if r.CustomVersion != "" {
		builder.WithCustomVersion(r.CustomVersion)
	}

	if r.ReleaseChannelName != "" {
		builder.WithReleaseChannel(r.ReleaseChannelName)
	}

	if r.EnableFederation {
		builder.WithFederation(r.FederationNonce, r.AllowedTeams, r.AllowedNamespaces, r.AllowedClusters)
	}

	return builder
}

// ToInstanceResponse converts an Instance to API response
func ToInstanceResponse(instance *unleash.Instance) *UnleashInstanceResponse {
	return &UnleashInstanceResponse{
		Name:                  instance.Name,
		Namespace:             instance.Namespace,
		CreatedAt:             instance.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Age:                   instance.Age(),
		Version:               instance.Version,
		Status:                instance.Status(),
		StatusLabel:           instance.StatusLabel(),
		APIUrl:                instance.APIUrl,
		WebUrl:                instance.WebUrl,
		VersionSource:         instance.VersionSource(),
		CustomVersion:         instance.CustomVersion,
		ReleaseChannelName:    instance.ReleaseChannelName,
		ResolvedImage:         instance.ResolvedImage,
		ChannelNameFromStatus: instance.ChannelNameFromStatus,
	}
}

// ToListResponse converts a list of instances to API response
func ToListResponse(instances []*unleash.Instance) *UnleashListResponse {
	responses := make([]*UnleashInstanceResponse, len(instances))
	for i, instance := range instances {
		responses[i] = ToInstanceResponse(instance)
	}
	return &UnleashListResponse{
		Instances: responses,
		Count:     len(responses),
	}
}
