package kubernetes

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIntentConfig() *unleash.Config {
	return &unleash.Config{
		Name:                      "team-a",
		ReleaseChannelName:        "stable",
		EnableFederation:          true,
		FederationNonce:           "abc12345",
		AllowedTeams:              "team-a,team-b",
		AllowedNamespaces:         "team-a,team-b",
		AllowedClusters:           "dev-gcp,prod-gcp",
		LogLevel:                  "warn",
		DatabasePoolMax:           3,
		DatabasePoolIdleTimeoutMs: 1000,
	}
}

// Every annotation bifrost writes must carry the version it was written under,
// otherwise the reader has nothing to check and is back to guessing.
func TestMarshalIntent_StampsSchemaVersion(t *testing.T) {
	intent, err := MarshalIntent(testIntentConfig())
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(intent), &raw))
	assert.EqualValues(t, IntentSchemaVersion, raw["schemaVersion"], "the annotation must record its schema")

	// The config's own fields stay at the top level, under the names they have
	// always used, so the payload is still readable by anything that knew the
	// pre-versioning format.
	assert.Equal(t, "team-a", raw["Name"])
	assert.EqualValues(t, 3, raw["DatabasePoolMax"])

	back, err := UnmarshalIntent(intent)
	require.NoError(t, err)
	assert.Equal(t, testIntentConfig(), back, "a round trip must not change the intent")
}

// The whole point of the version is that an annotation this build cannot vouch
// for is refused rather than read into a half-defaulted config and rendered
// onto a live instance.
func TestUnmarshalIntent_RefusesUnrecognisedSchemaVersions(t *testing.T) {
	for _, tc := range []struct {
		name, annotation string
		wantErr          string
	}{
		{
			// Annotations written before the version field existed. That format
			// is field-for-field what version 1 writes, so it is read as 1 —
			// an explicit allowance, not an oversight, and one that lapses the
			// moment the schema moves past 1.
			name:       "unversioned is read as version 1",
			annotation: `{"Name":"team-a","LogLevel":"warn","DatabasePoolMax":3}`,
		},
		{
			name:       "current version is accepted",
			annotation: `{"schemaVersion":1,"Name":"team-a","LogLevel":"warn","DatabasePoolMax":3}`,
		},
		{
			// Two bifrost versions overlap on every rolling deploy, so the new
			// pod's annotations really do land in front of the old pod's reader.
			name:       "a newer schema than this build knows",
			annotation: `{"schemaVersion":2,"Name":"team-a","LogLevel":"warn","DatabasePoolMax":3}`,
			wantErr:    "schema version 2",
		},
		{
			name:       "a schema this build has never heard of",
			annotation: `{"schemaVersion":99,"Name":"team-a","LogLevel":"warn","DatabasePoolMax":3}`,
			wantErr:    "schema version 99",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := UnmarshalIntent(tc.annotation)

			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "team-a", cfg.Name)
				return
			}

			require.Error(t, err, "an unreadable annotation must not be turned into a config")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, cfg, "nothing partially parsed may escape to the caller")
		})
	}
}

// The hazard this guards is a field the writing schema did not have. json
// zero-fills it without complaint, and because the annotation is authoritative
// the reconciler would render that zero onto every instance still carrying the
// old annotation. Under an unrecognised version it never gets that far.
func TestUnmarshalIntent_RefusesAnnotationMissingAFieldThisSchemaExpects(t *testing.T) {
	// Same payload twice: once claiming a schema this build reads, once
	// claiming one it does not.
	const missingPoolMax = `"Name":"team-a","LogLevel":"warn"`

	readable, err := UnmarshalIntent("{" + missingPoolMax + "}")
	require.NoError(t, err)
	assert.Zero(t, readable.DatabasePoolMax,
		"under a schema this build reads, a missing field is silently zero — which is exactly why the version has to be checked")

	_, err = UnmarshalIntent(`{"schemaVersion":2,` + missingPoolMax + `}`)
	require.Error(t, err, "a foreign schema must be refused before its fields are believed")
	assert.Contains(t, err.Error(), "schema version 2")
}

// The version is only worth anything if it moves when the schema does. Adding,
// removing, renaming or retyping a field in unleash.Config changes what an old
// annotation means, so it has to come with a bump — and this list is where you
// find out you forgot.
func TestIntentSchemaVersionPinsConfigFields(t *testing.T) {
	// Keyed by schema version, so the obvious mechanical response to a failure —
	// paste the new field into the list — is not enough on its own. Adding a
	// field without bumping IntentSchemaVersion leaves no entry for the new
	// version and fails here, which is the whole point: an unbumped schema means
	// every existing annotation still reads as current, gets zero-filled, and is
	// rendered back onto every live instance carrying one.
	fieldsByVersion := map[int][]string{1: {
		"Name string `json:\"Name\"`",
		"CustomVersion string `json:\"CustomVersion\"`",
		"ReleaseChannelName string `json:\"ReleaseChannelName\"`",
		"EnableFederation bool `json:\"EnableFederation\"`",
		"FederationNonce string `json:\"FederationNonce\"`",
		"AllowedTeams string `json:\"AllowedTeams\"`",
		"AllowedNamespaces string `json:\"AllowedNamespaces\"`",
		"AllowedClusters string `json:\"AllowedClusters\"`",
		"LogLevel string `json:\"LogLevel\"`",
		"DatabasePoolMax int `json:\"DatabasePoolMax\"`",
		"DatabasePoolIdleTimeoutMs int `json:\"DatabasePoolIdleTimeoutMs\"`",
	}}

	want, ok := fieldsByVersion[IntentSchemaVersion]
	require.True(t, ok,
		"no field list pinned for schema version %d: bump the version and add its entry together, or an old annotation is silently read as current",
		IntentSchemaVersion)

	typ := reflect.TypeOf(unleash.Config{})
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		f := typ.Field(i)
		got = append(got, strings.TrimSpace(f.Name+" "+f.Type.String()+" `"+string(f.Tag)+"`"))
	}

	assert.Equal(t, want, got,
		"unleash.Config's fields changed: bump kubernetes.IntentSchemaVersion so annotations written under the old schema are refused instead of zero-filled, then add a new entry rather than editing this one")
}

// The list above compares the struct to a list, so updating both together slips
// a new field past it. This tests the hazard itself instead: a frozen schema-1
// annotation, written out when the schema was defined, must still populate every
// field of the config this build reads.
//
// Add a field without bumping the version and that field is absent from the
// frozen payload, so it deserialises to its zero value — which is precisely what
// then gets rendered onto every live instance carrying an old annotation. There
// is no way to satisfy this by editing a list: either bump the version, or
// change what schema 1 has always meant, and the latter is visible in review.
func TestSchemaOneAnnotationStillFillsEveryField(t *testing.T) {
	if IntentSchemaVersion != 1 {
		t.Skipf("frozen payload describes schema 1, build reads %d — freeze one for the new version too", IntentSchemaVersion)
	}

	const frozenSchemaOne = `{"schemaVersion":1,` +
		`"Name":"team-a",` +
		`"CustomVersion":"v5.1.2",` +
		`"ReleaseChannelName":"stable",` +
		`"EnableFederation":true,` +
		`"FederationNonce":"abcd1234",` +
		`"AllowedTeams":"team-a,team-b",` +
		`"AllowedNamespaces":"ns-a,ns-b",` +
		`"AllowedClusters":"dev-gcp,prod-gcp",` +
		`"LogLevel":"warn",` +
		`"DatabasePoolMax":7,` +
		`"DatabasePoolIdleTimeoutMs":900}`

	cfg, err := UnmarshalIntent(frozenSchemaOne)
	require.NoError(t, err)

	v := reflect.ValueOf(*cfg)
	typ := v.Type()
	for i := range typ.NumField() {
		assert.Falsef(t, v.Field(i).IsZero(),
			"%s is zero after reading a schema-1 annotation: the field is not in schema 1, so every existing "+
				"annotation zero-fills it and the reconciler renders that zero onto live instances — bump IntentSchemaVersion",
			typ.Field(i).Name)
	}
}

// Rendering is what actually writes the annotation, so the version has to reach
// the CRD and not just the marshaller.
func TestStampManagedMetadata_WritesVersionedIntent(t *testing.T) {
	server := &unleashv1.Unleash{}
	stampManagedMetadata(server, testIntentConfig())

	intent := server.GetAnnotations()[AnnotationDesiredState]
	require.NotEmpty(t, intent, "the rendered CRD must carry the intent")
	assert.Contains(t, intent, `"schemaVersion":1`)

	cfg, err := UnmarshalIntent(intent)
	require.NoError(t, err, "what rendering writes must be what reading accepts")
	assert.Equal(t, testIntentConfig(), cfg)
}
