package config

import (
	"context"
	"os"
	"os/exec"
	"reflect"
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

// validConfig populates every `,required` field, because Validate now audits all
// of them rather than the four somebody once listed.
func validConfig() *Config {
	return &Config{
		Google: GoogleConfig{ProjectID: "nais-management"},
		Unleash: UnleashConfig{
			InstanceNamespace:           "bifrost-unleash",
			InstanceServiceaccount:      "bifrost-unleash-sa",
			SQLInstanceID:               "unleash-sql",
			SQLInstanceRegion:           "europe-north1",
			SQLInstanceAddress:          "10.0.0.1",
			InstanceWebIngressHost:      "unleash-web.example",
			InstanceWebIngressClass:     "external-fa-haproxy",
			InstanceWebOAuthJWTAudience: "audience",
			InstanceAPIIngressHost:      "unleash-api.example",
			InstanceAPIIngressClass:     "internal-haproxy",
			TeamsApiURL:                 "https://console.example/graphql",
			TeamsApiSecretName:          "teams-api",
			TeamsApiSecretTokenKey:      "token",
		},
	}
}

// requiredEnvNames lists the variables the struct tags mark `,required`, so the
// tests below assert against the tags rather than against a second hand-written
// list that could drift from the first in the same way.
func requiredEnvNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		field := t.Field(i)
		if field.Type.Kind() == reflect.Struct {
			names = append(names, requiredEnvNames(field.Type)...)
			continue
		}
		if name, required := requiredEnvName(field); required {
			names = append(names, name)
		}
	}
	return names
}

// setEnvField sets the string field reading the named variable, so a test can
// blank or pad one field by the name an operator would recognise.
func setEnvField(t *testing.T, v reflect.Value, env, value string) {
	t.Helper()
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if field.Type.Kind() == reflect.Struct {
			setEnvField(t, v.Field(i), env, value)
			continue
		}
		if name, _ := requiredEnvName(field); name == env {
			v.Field(i).SetString(value)
		}
	}
}

// The list this replaced audited 4 of 14 `,required` fields and had no way to
// notice a fifteenth being added. Asserting the property instead of the list is
// the point: every required field, blanked, has to be refused by name.
func TestValidate_AuditsEveryRequiredField(t *testing.T) {
	names := requiredEnvNames(reflect.TypeOf(Config{}))
	if len(names) < 14 {
		t.Fatalf("found %d required fields (%v), want at least the 14 that exist; the walk is not finding them", len(names), names)
	}

	for _, env := range names {
		t.Run(env, func(t *testing.T) {
			c := validConfig()
			setEnvField(t, reflect.ValueOf(c).Elem(), env, "")

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil with %s empty", env)
			}
			if !strings.Contains(err.Error(), env) {
				t.Errorf("Validate() error = %q, want it to name %s", err, env)
			}
		})
	}
}

// A padded value is not trimmed anywhere near all of its readers: the reconciler
// informer and the adopter would use one namespace name and every other
// namespaced call the other.
func TestValidate_RejectsWhitespacePaddedValues(t *testing.T) {
	c := validConfig()
	c.Unleash.InstanceNamespace = " bifrost-unleash "

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for a whitespace-padded namespace")
	}
	if !strings.Contains(err.Error(), "BIFROST_UNLEASH_INSTANCE_NAMESPACE") || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("Validate() error = %q, want it to name the variable and the whitespace", err)
	}
}

// Adoption stamps instances that have no desired-state annotation. A reconciler
// allowed to write is then one relaxed rule away from converging all of them
// from a lossy read-back of their own specs, so the pair is refused at startup
// rather than guarded only where the writes happen.
func TestValidate_RefusesAdoptionWithWritesEnabled(t *testing.T) {
	c := validConfig()
	c.Reconciler.AutoAdopt = true
	c.Reconciler.DryRun = false

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for autoAdopt with dry-run off")
	}
	for _, want := range []string{"BIFROST_RECONCILER_AUTO_ADOPT", "BIFROST_RECONCILER_DRY_RUN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %q, want it to name %s", err, want)
		}
	}

	// And the intended rollout combination stays legal.
	c.Reconciler.DryRun = true
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v for autoAdopt with dry-run on, want nil", err)
	}
}

// Validate is only a guard if New calls it. New exits the process on a bad
// config, so the call is asserted from a subprocess: without it, New returns a
// config with an empty instance namespace and the child exits 0.
func TestNew_RefusesAnInvalidConfiguration(t *testing.T) {
	if os.Getenv("BIFROST_TEST_NEW_SUBPROCESS") == "1" {
		New(context.Background())
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestNew_RefusesAnInvalidConfiguration", "-test.v")
	cmd.Env = append(os.Environ(),
		"BIFROST_TEST_NEW_SUBPROCESS=1",
		// Set, so envconfig's own `,required` is satisfied and the empty
		// namespace is the only thing left to catch.
		"BIFROST_UNLEASH_INSTANCE_NAMESPACE=",
		"BIFROST_GOOGLE_PROJECT_ID=nais-management",
		"BIFROST_UNLEASH_INSTANCE_SERVICEACCOUNT=bifrost-unleash-sa",
		"BIFROST_UNLEASH_SQL_INSTANCE_ID=unleash-sql",
		"BIFROST_UNLEASH_SQL_INSTANCE_REGION=europe-north1",
		"BIFROST_UNLEASH_SQL_INSTANCE_ADDRESS=10.0.0.1",
		"BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_HOST=web.example",
		"BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_CLASS=web-class",
		"BIFROST_UNLEASH_INSTANCE_WEB_OAUTH_JWT_AUDIENCE=audience",
		"BIFROST_UNLEASH_INSTANCE_API_INGRESS_HOST=api.example",
		"BIFROST_UNLEASH_INSTANCE_API_INGRESS_CLASS=api-class",
		"BIFROST_UNLEASH_INSTANCE_TEAMS_API_URL=https://console.example/graphql",
		"BIFROST_UNLEASH_INSTANCE_TEAMS_API_SECRET_NAME=teams-api",
		"BIFROST_UNLEASH_INSTANCE_TEAMS_API_TOKEN_SECRET_KEY=token",
	)

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("config.New accepted a configuration Validate rejects; output:\n%s", out)
	}
	if !strings.Contains(string(out), "BIFROST_UNLEASH_INSTANCE_NAMESPACE") {
		t.Errorf("config.New failed without naming the offending variable; output:\n%s", out)
	}
}
