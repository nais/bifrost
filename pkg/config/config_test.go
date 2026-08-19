package config

import (
	"context"
	"strings"
	"testing"

	envconfig "github.com/sethvargo/go-envconfig"
)

// The whole point of Validate: `,required` passes a set-but-empty variable, so
// the config parses cleanly and the empty value is only noticed downstream —
// where an empty namespace means every namespace.
func TestValidate_RejectsSetButEmptyRequiredValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  string
		mut  func(*Config)
	}{
		{"instance namespace", "BIFROST_UNLEASH_INSTANCE_NAMESPACE", func(c *Config) { c.Unleash.InstanceNamespace = "" }},
		{"google project", "BIFROST_GOOGLE_PROJECT_ID", func(c *Config) { c.Google.ProjectID = "" }},
		{"sql instance", "BIFROST_UNLEASH_SQL_INSTANCE_ID", func(c *Config) { c.Unleash.SQLInstanceID = "" }},
		{"instance serviceaccount", "BIFROST_UNLEASH_INSTANCE_SERVICEACCOUNT", func(c *Config) { c.Unleash.InstanceServiceaccount = "" }},
		{"whitespace only namespace", "BIFROST_UNLEASH_INSTANCE_NAMESPACE", func(c *Config) { c.Unleash.InstanceNamespace = "   " }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mut(c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error for empty %s", tt.env)
			}
			if !strings.Contains(err.Error(), tt.env) {
				t.Errorf("Validate() error = %q, want it to name %s", err, tt.env)
			}
		})
	}
}

func TestValidate_AcceptsAFullyPopulatedConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// envconfig's `,required` only asserts the variable was found. This pins that
// behaviour so a library upgrade that starts rejecting empty values shows up
// here rather than silently making Validate look redundant.
func TestEnvconfigRequiredAcceptsEmptyValues(t *testing.T) {
	t.Setenv("BIFROST_UNLEASH_INSTANCE_NAMESPACE", "")

	var u UnleashConfig
	err := envconfig.Process(context.Background(), &u)

	// Processing stops at the *next* required field, which proves the empty
	// namespace itself was accepted.
	if err != nil && strings.Contains(err.Error(), "BIFROST_UNLEASH_INSTANCE_NAMESPACE") {
		t.Fatalf("envconfig now rejects a set-but-empty required value: %v", err)
	}
	if u.InstanceNamespace != "" {
		t.Fatalf("InstanceNamespace = %q, want empty", u.InstanceNamespace)
	}
}

func validConfig() *Config {
	return &Config{
		Google: GoogleConfig{ProjectID: "nais-management"},
		Unleash: UnleashConfig{
			InstanceNamespace:      "bifrost-unleash",
			InstanceServiceaccount: "bifrost-unleash-sa",
			SQLInstanceID:          "unleash-sql",
		},
	}
}
