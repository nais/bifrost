package kubernetes

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
)

const (
	// LabelManagedBy marks which controller owns an Unleash instance. Bifrost
	// only reconciles instances it manages, identified by this label, so the
	// reconciler never touches hand-authored or foreign Unleash CRs.
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	ManagedByBifrost = "bifrost"

	// AnnotationDesiredState carries the bifrost per-instance intent (the
	// unleash.Config) as JSON, alongside the schemaVersion it was written
	// under. It is the authoritative, non-lossy source of truth the reconciler
	// re-renders from — unlike reverse-engineering the rendered spec via
	// LoadConfigFromCRD, which drops fields.
	AnnotationDesiredState = "bifrost.nais.io/desired-state"

	// AnnotationChannelMigration carries the Bifrost-owned, versioned channel
	// migration transaction. The migration state machine updates it separately
	// with annotation-only optimistic-lock patches.
	AnnotationChannelMigration = "bifrost.nais.io/channel-migration"
	// AnnotationAdoption is a CR-local, short-lived marker. Its only valid
	// value is "pending", until Unleasherator reports the canonical CR healthy
	// for its current generation.
	AnnotationAdoption = "bifrost.nais.io/adoption"
)

// IntentSchemaVersion adds explicit resource ownership. Readers also accept v0
// and v1, whose absent profile must not acquire that ownership.
const IntentSchemaVersion = 2

// intentEnvelope is the on-annotation shape: the config's own fields with the
// schema version alongside them. Config is embedded rather than nested so the
// payload stays byte-for-byte what pre-versioning bifrost wrote, with the
// schemaVersion key as the only addition.
type intentEnvelope struct {
	SchemaVersion int `json:"schemaVersion"`
	unleash.Config
}

// MarshalIntent serializes a per-instance config, stamped with the current
// schema version, for storage in the desired-state annotation.
func MarshalIntent(cfg *unleash.Config) (string, error) {
	version := IntentSchemaVersion
	if cfg.ResourceProfile == "" {
		// Keep legacy migration transaction hashes byte-compatible.
		version = 1
	}
	b, err := json.Marshal(intentEnvelope{SchemaVersion: version, Config: *cfg})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalIntent parses the desired-state annotation back into a config, and
// refuses one written under a schema this build does not know how to read.
//
// Refusing is the point: the caller treats the error like malformed JSON and
// skips the instance, which leaves it as it is. Reading it anyway would render
// a partially zero-filled config onto a running instance, and #581's validation
// only catches the degenerate cases — a config zero-filled in one field still
// validates and would be applied verbatim.
func UnmarshalIntent(s string) (*unleash.Config, error) {
	envelope := intentEnvelope{}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, err
	}

	version := envelope.SchemaVersion
	if version != 0 && version != 1 && version != 2 {
		return nil, fmt.Errorf("desired-state annotation has schema version %d, this build reads %d", version, IntentSchemaVersion)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("desired-state annotation must be an object")
	}
	if raw, present := fields["schemaVersion"]; present && string(raw) == "null" {
		return nil, fmt.Errorf("desired-state schema version must be an integer")
	}
	for key := range fields {
		if (strings.EqualFold(key, "ResourceProfile") && key != "ResourceProfile") ||
			(strings.EqualFold(key, "schemaVersion") && key != "schemaVersion") {
			return nil, fmt.Errorf("noncanonical desired-state field %q", key)
		}
	}
	if _, present := fields["ResourceProfile"]; present {
		if version != 2 || !unleash.ValidResourceProfile(envelope.ResourceProfile) {
			return nil, fmt.Errorf("invalid resource profile in desired-state schema %d", version)
		}
	}

	cfg := envelope.Config
	return &cfg, nil
}

// IsManagedByBifrost reports whether the reconciler owns this instance.
func IsManagedByBifrost(crd *unleashv1.Unleash) bool {
	return crd.GetLabels()[LabelManagedBy] == ManagedByBifrost
}

// ApplyManagedMetadata copies Bifrost's rendered managed-by label and
// desired-state annotation onto a live CRD. It leaves finalizers,
// ownerReferences, foreign metadata, and temporary migration markers alone.
func ApplyManagedMetadata(live, rendered *unleashv1.Unleash) {
	if live.Labels == nil {
		live.Labels = map[string]string{}
	}
	live.Labels[LabelManagedBy] = ManagedByBifrost

	// Absent only when marshaling the intent failed; keeping whatever the live
	// object already carries beats replacing it with nothing.
	if intent := rendered.GetAnnotations()[AnnotationDesiredState]; intent != "" {
		if IntentEqual(live.GetAnnotations()[AnnotationDesiredState], intent) {
			return
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[AnnotationDesiredState] = intent
	}
}

// IntentEqual ignores serialization and schema stamping, not configuration.
func IntentEqual(a, b string) bool {
	left, err := UnmarshalIntent(a)
	if err != nil {
		return false
	}
	right, err := UnmarshalIntent(b)
	return err == nil && reflect.DeepEqual(left, right)
}

// stampManagedMetadata sets the managed-by label and desired-state annotation on
// a rendered CRD so every create/update marks the instance as bifrost-managed
// and records the intent it was rendered from.
func stampManagedMetadata(server *unleashv1.Unleash, cfg *unleash.Config) {
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels[LabelManagedBy] = ManagedByBifrost

	// Best-effort: if the intent cannot be marshaled, the reconciler falls back
	// to LoadConfigFromCRD, so we simply omit the annotation rather than fail
	// rendering.
	if intent, err := MarshalIntent(cfg); err == nil {
		if server.Annotations == nil {
			server.Annotations = map[string]string{}
		}
		server.Annotations[AnnotationDesiredState] = intent
	}
}
