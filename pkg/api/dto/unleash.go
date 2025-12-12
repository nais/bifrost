package dto

import (
	"github.com/nais/bifrost/pkg/domain/unleash"
)

// UnleashConfigRequest represents the JSON request body for creating/updating an Unleash instance
type UnleashConfigRequest struct {
	Name                      string `json:"name,omitempty"`
	CustomVersion             string `json:"custom-version,omitempty"`
	ReleaseChannelName        string `json:"release-channel-name,omitempty"` // v1 only
	EnableFederation          bool   `json:"enable-federation,omitempty"`
	AllowedTeams              string `json:"allowed-teams,omitempty"`
	AllowedNamespaces         string `json:"allowed-namespaces,omitempty"`
	AllowedClusters           string `json:"allowed-clusters,omitempty"`
	LogLevel                  string `json:"log-level,omitempty"`
	DatabasePoolMax           int    `json:"database-pool-max,omitempty"`
	DatabasePoolIdleTimeoutMs int    `json:"database-pool-idle-timeout-ms,omitempty"`
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

	// Version source (v1 only, computed field)
	VersionSource      string `json:"version_source,omitempty"`
	CustomVersion      string `json:"custom_version,omitempty"`
	ReleaseChannelName string `json:"release_channel_name,omitempty"` // v1 only

	// Read-only status (v1 only)
	ResolvedImage         string `json:"resolved_image,omitempty"`           // v1 only
	ChannelNameFromStatus string `json:"channel_name_from_status,omitempty"` // v1 only
}

// UnleashListResponse represents a list of Unleash instances
type UnleashListResponse struct {
	Instances []*UnleashInstanceResponse `json:"instances"`
	Count     int                        `json:"count"`
}

// ToConfigBuilder converts a request DTO to a domain ConfigBuilder
func (r *UnleashConfigRequest) ToConfigBuilder() *unleash.ConfigBuilder {
	builder := unleash.NewConfigBuilder().
		WithName(r.Name).
		WithLogLevel(r.LogLevel).
		WithDatabasePool(r.DatabasePoolMax, r.DatabasePoolIdleTimeoutMs)

	if r.CustomVersion != "" {
		builder.WithCustomVersion(r.CustomVersion)
	}

	if r.ReleaseChannelName != "" {
		builder.WithReleaseChannel(r.ReleaseChannelName)
	}

	if r.EnableFederation {
		builder.WithFederation("", r.AllowedTeams, r.AllowedNamespaces, r.AllowedClusters)
	}

	return builder
}

// ToV0Response converts an Instance to v0 API response (no channel fields)
func ToV0Response(instance *unleash.Instance) *UnleashInstanceResponse {
	return &UnleashInstanceResponse{
		Name:          instance.Name,
		Namespace:     instance.Namespace,
		CreatedAt:     instance.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Age:           instance.Age(),
		Version:       instance.Version,
		Status:        instance.Status(),
		StatusLabel:   instance.StatusLabel(),
		APIUrl:        instance.APIUrl,
		WebUrl:        instance.WebUrl,
		CustomVersion: instance.CustomVersion,
		// Explicitly omit channel fields for v0
	}
}

// ToV1Response converts an Instance to v1 API response (includes channel fields)
func ToV1Response(instance *unleash.Instance) *UnleashInstanceResponse {
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

// ToV0ListResponse converts a list of instances to v0 API response
func ToV0ListResponse(instances []*unleash.Instance) *UnleashListResponse {
	responses := make([]*UnleashInstanceResponse, len(instances))
	for i, instance := range instances {
		responses[i] = ToV0Response(instance)
	}
	return &UnleashListResponse{
		Instances: responses,
		Count:     len(responses),
	}
}

// ToV1ListResponse converts a list of instances to v1 API response
func ToV1ListResponse(instances []*unleash.Instance) *UnleashListResponse {
	responses := make([]*UnleashInstanceResponse, len(instances))
	for i, instance := range instances {
		responses[i] = ToV1Response(instance)
	}
	return &UnleashListResponse{
		Instances: responses,
		Count:     len(responses),
	}
}
