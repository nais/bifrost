package unleash

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	validator "github.com/go-playground/validator/v10"
	"github.com/nais/bifrost/pkg/infrastructure/github"
)

var (
	// ErrBothVersionSourcesSet is returned when both CustomVersion and ReleaseChannelName are set
	ErrBothVersionSourcesSet = errors.New("cannot set both customVersion and releaseChannelName")

	// ErrInvalidHostname is returned when the name is not a valid hostname
	ErrInvalidHostname = errors.New("name must be a valid hostname")

	// ErrInvalidLogLevel is returned when log level is invalid
	ErrInvalidLogLevel = errors.New("log level must be one of: debug, info, warn, error, fatal, panic")

	// ErrInvalidDatabasePool is returned when database pool settings are invalid
	ErrInvalidDatabasePool = errors.New("database pool max must be between 1 and 10")
)

// Config represents the configuration for an Unleash instance
type Config struct {
	// Identity
	Name string

	// Version configuration (mutually exclusive)
	CustomVersion      string
	ReleaseChannelName string

	// Federation settings
	EnableFederation  bool
	FederationNonce   string
	AllowedTeams      string
	AllowedNamespaces string
	AllowedClusters   string

	// Operational settings
	LogLevel                  string
	DatabasePoolMax           int
	DatabasePoolIdleTimeoutMs int
}

// ConfigBuilder provides a builder pattern for creating Config instances
type ConfigBuilder struct {
	config *Config
	errors []error
}

// NewConfigBuilder creates a new ConfigBuilder
func NewConfigBuilder() *ConfigBuilder {
	return &ConfigBuilder{
		config: &Config{
			// Defaults
			LogLevel:                  "warn",
			DatabasePoolMax:           3,
			DatabasePoolIdleTimeoutMs: 1000,
		},
		errors: []error{},
	}
}

// WithName sets the instance name
func (b *ConfigBuilder) WithName(name string) *ConfigBuilder {
	b.config.Name = name
	return b
}

// WithCustomVersion sets a custom version (mutually exclusive with release channel)
func (b *ConfigBuilder) WithCustomVersion(version string) *ConfigBuilder {
	b.config.CustomVersion = version
	b.config.ReleaseChannelName = "" // Clear channel if set
	return b
}

// WithReleaseChannel sets a release channel (mutually exclusive with custom version)
func (b *ConfigBuilder) WithReleaseChannel(channelName string) *ConfigBuilder {
	b.config.ReleaseChannelName = channelName
	b.config.CustomVersion = "" // Clear custom version if set
	return b
}

// WithFederation enables federation with the given settings
func (b *ConfigBuilder) WithFederation(nonce, teams, namespaces, clusters string) *ConfigBuilder {
	b.config.EnableFederation = true
	b.config.FederationNonce = nonce
	b.config.AllowedTeams = teams
	b.config.AllowedNamespaces = namespaces
	b.config.AllowedClusters = clusters
	return b
}

// WithLogLevel sets the log level
func (b *ConfigBuilder) WithLogLevel(level string) *ConfigBuilder {
	b.config.LogLevel = level
	return b
}

// WithDatabasePool sets the database pool configuration
func (b *ConfigBuilder) WithDatabasePool(max, idleTimeoutMs int) *ConfigBuilder {
	b.config.DatabasePoolMax = max
	b.config.DatabasePoolIdleTimeoutMs = idleTimeoutMs
	return b
}

// SetDefaultVersionIfNeeded sets a default custom version from available versions if no version source is configured
func (b *ConfigBuilder) SetDefaultVersionIfNeeded(availableVersions []github.UnleashVersion) *ConfigBuilder {
	if b.config.CustomVersion == "" && b.config.ReleaseChannelName == "" && len(availableVersions) > 0 {
		b.config.CustomVersion = availableVersions[0].GitTag
	}
	return b
}

// MergeTeamsAndNamespaces merges teams and namespaces into a single list
func (b *ConfigBuilder) MergeTeamsAndNamespaces() *ConfigBuilder {
	merged := make(map[string]bool)

	for _, team := range splitNoEmpty(b.config.AllowedTeams, ",") {
		merged[strings.TrimSpace(team)] = true
	}

	for _, namespace := range splitNoEmpty(b.config.AllowedNamespaces, ",") {
		merged[strings.TrimSpace(namespace)] = true
	}

	result := make([]string, 0, len(merged))
	for key := range merged {
		result = append(result, key)
	}

	sort.Strings(result)

	mergedStr := strings.Join(result, ",")
	b.config.AllowedTeams = mergedStr
	b.config.AllowedNamespaces = mergedStr

	return b
}

// Build validates and returns the Config, or returns an error if validation fails
func (b *ConfigBuilder) Build() (*Config, error) {
	// Validate mutual exclusivity
	if b.config.CustomVersion != "" && b.config.ReleaseChannelName != "" {
		return nil, ErrBothVersionSourcesSet
	}

	// Validate hostname
	hostnameRegex := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	if !hostnameRegex.MatchString(b.config.Name) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidHostname, b.config.Name)
	}

	// Validate log level
	validLogLevels := map[string]bool{
		"debug": true, "info": true, "warn": true, "error": true, "fatal": true, "panic": true,
	}
	if !validLogLevels[b.config.LogLevel] {
		return nil, ErrInvalidLogLevel
	}

	// Validate database pool
	if b.config.DatabasePoolMax < 1 || b.config.DatabasePoolMax > 10 {
		return nil, ErrInvalidDatabasePool
	}

	// Use validator for struct validation
	validate := validator.New(validator.WithRequiredStructEnabled())
	if err := validate.Struct(b.config); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return b.config, nil
}

// VersionSource returns the source of the version configuration
func (c *Config) VersionSource() string {
	if c.CustomVersion != "" {
		return "custom"
	}
	if c.ReleaseChannelName != "" {
		return "releaseChannel"
	}
	return "default"
}

// splitNoEmpty splits a string by delimiter and returns non-empty parts
func splitNoEmpty(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, sep)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// LoadFromExisting creates a ConfigBuilder from an existing Instance
func LoadFromExisting(instance *Instance) *ConfigBuilder {
	return &ConfigBuilder{
		config: &Config{
			Name:                      instance.Name,
			CustomVersion:             instance.CustomVersion,
			ReleaseChannelName:        instance.ReleaseChannelName,
			EnableFederation:          false, // Will be populated from CRD
			FederationNonce:           "",    // Will be populated from CRD
			AllowedTeams:              "",    // Will be populated from CRD
			AllowedNamespaces:         "",    // Will be populated from CRD
			AllowedClusters:           "",    // Will be populated from CRD
			LogLevel:                  "warn",
			DatabasePoolMax:           3,
			DatabasePoolIdleTimeoutMs: 1000,
		},
		errors: []error{},
	}
}
