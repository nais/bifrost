package dto

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// UnleashConfigRequest represents the JSON request body for creating/updating an Unleash instance
type UnleashConfigRequest struct {
	ResourceProfile    *string `json:"resource_profile,omitempty"`
	ProfileOnly        bool    `json:"-"`
	Name               string  `json:"name,omitempty"`
	ReleaseChannelName string  `json:"release_channel_name,omitempty"`
	EnableFederation   bool    `json:"enable_federation,omitempty"`
	FederationNonce    string  `json:"-"` // Internal use only, not exposed in API
	// AllowedTeams is a pointer so that a supplied-but-empty list (revoking the
	// last team) is distinguishable from an omitted field (no statement about
	// access). A plain string collapses the two and makes the list un-emptiable.
	AllowedTeams    *string `json:"allowed_teams,omitempty"`
	AllowedClusters *string `json:"allowed_clusters,omitempty"`

	// Deprecated: Use release_channel_name instead
	CustomVersion string `json:"custom_version,omitempty" swaggerignore:"true"`
	// Deprecated: Use allowed_teams instead (teams and namespaces are merged)
	AllowedNamespaces         *string `json:"allowed_namespaces,omitempty" swaggerignore:"true"`
	LogLevel                  string  `json:"log_level,omitempty"`
	DatabasePoolMax           int     `json:"database_pool_max,omitempty"`
	DatabasePoolIdleTimeoutMs int     `json:"database_pool_idle_timeout_ms,omitempty"`
}

func (r *UnleashConfigRequest) UnmarshalJSON(data []byte) error {
	type request UnleashConfigRequest
	var decoded request
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "resource_profile") && key != "resource_profile" {
			return fmt.Errorf("noncanonical resource_profile field %q", key)
		}
	}
	if _, present := fields["resource_profile"]; present {
		if decoded.ResourceProfile == nil || !unleash.ValidResourceProfile(*decoded.ResourceProfile) {
			return fmt.Errorf("resource_profile must be small, medium or large, not empty or null")
		}
		// Match the update/builder predicates, not raw keys that may be ignored.
		decoded.ProfileOnly = decoded.ReleaseChannelName == "" && decoded.CustomVersion == "" &&
			decoded.AllowedTeams == nil && decoded.AllowedNamespaces == nil && decoded.AllowedClusters == nil &&
			decoded.LogLevel == "" && decoded.DatabasePoolMax <= 0 && decoded.DatabasePoolIdleTimeoutMs <= 0
	}
	*r = UnleashConfigRequest(decoded)
	return nil
}

// UnleashResponse preserves the CRD response shape used by existing clients.
type UnleashResponse struct {
	*unleashv1.Unleash
	ResourceProfile         string                      `json:"resource_profile"`
	ResourceProfileExplicit bool                        `json:"resource_profile_explicit"`
	ResourceProfileMatches  bool                        `json:"resource_profile_matches"`
	ResolvedResources       corev1.ResourceRequirements `json:"resolved_resources"`
}

func ToCRDResponse(crd *unleashv1.Unleash) *UnleashResponse {
	if crd == nil {
		return nil
	}
	profile := ""
	if cfg, err := kubernetes.UnmarshalIntent(crd.GetAnnotations()[kubernetes.AnnotationDesiredState]); err == nil {
		profile = cfg.ResourceProfile
	}
	return &UnleashResponse{
		Unleash:                 crd,
		ResourceProfile:         unleash.EffectiveResourceProfile(profile),
		ResourceProfileExplicit: profile != "",
		ResourceProfileMatches:  unleash.ResourcesMatchProfile(crd.Spec.Resources, profile),
		ResolvedResources:       *crd.Spec.Resources.DeepCopy(),
	}
}

// UnleashInstanceResponse represents the JSON response for an Unleash instance
type UnleashInstanceResponse struct {
	ResourceProfile         string                      `json:"resource_profile"`
	ResourceProfileExplicit bool                        `json:"resource_profile_explicit"`
	ResourceProfileMatches  bool                        `json:"resource_profile_matches"`
	ResolvedResources       corev1.ResourceRequirements `json:"resolved_resources"`
	Name                    string                      `json:"name"`
	Namespace               string                      `json:"namespace"`
	CreatedAt               string                      `json:"created_at"`
	Age                     string                      `json:"age"`
	Version                 string                      `json:"version"`
	Status                  string                      `json:"status"`
	StatusLabel             string                      `json:"status_label"`
	APIUrl                  string                      `json:"api_url"`
	WebUrl                  string                      `json:"web_url"`

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
	if r.ResourceProfile != nil {
		builder.WithResourceProfile(*r.ResourceProfile)
	}

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
		builder.WithFederation(r.FederationNonce, derefString(r.AllowedTeams), derefString(r.AllowedNamespaces), derefString(r.AllowedClusters))
	}

	return builder
}

// derefString returns the pointed-to value, or the empty string when the field
// was omitted from the request.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ToInstanceResponse converts an Instance to API response
func ToInstanceResponse(instance *unleash.Instance) *UnleashInstanceResponse {
	return &UnleashInstanceResponse{
		ResourceProfile:         unleash.EffectiveResourceProfile(instance.ResourceProfile),
		ResourceProfileExplicit: instance.ResourceProfile != "",
		ResourceProfileMatches:  unleash.ResourcesMatchProfile(instance.Resources, instance.ResourceProfile),
		ResolvedResources:       instance.Resources,
		Name:                    instance.Name,
		Namespace:               instance.Namespace,
		CreatedAt:               instance.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Age:                     instance.Age(),
		Version:                 instance.Version,
		Status:                  instance.Status(),
		StatusLabel:             instance.StatusLabel(),
		APIUrl:                  instance.APIUrl,
		WebUrl:                  instance.WebUrl,
		VersionSource:           instance.VersionSource(),
		CustomVersion:           instance.CustomVersion,
		ReleaseChannelName:      instance.ReleaseChannelName,
		ResolvedImage:           instance.ResolvedImage,
		ChannelNameFromStatus:   instance.ChannelNameFromStatus,
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
