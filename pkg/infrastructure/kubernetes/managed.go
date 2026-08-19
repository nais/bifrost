package kubernetes

import (
	"encoding/json"
	"fmt"

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

	// LabelAdopt opts an instance out of automatic adoption. It is a label
	// rather than an annotation because the adopter's own List is label-scoped
	// and because `kubectl get unleash -l bifrost.nais.io/adopt=false` is then
	// the exemption list.
	//
	// Only the exact value AdoptOptOut exempts an instance; absence, an empty
	// value, or anything unrecognised leaves it eligible. That direction is
	// deliberate: adoption only adds a label and is undone by removing it, so a
	// mistyped exemption costs a label that can be deleted, whereas
	// presence-only semantics would make `adopt: "true"` silently mean the
	// opposite of what it reads as.
	LabelAdopt  = "bifrost.nais.io/adopt"
	AdoptOptOut = "false"
)

// IntentSchemaVersion is the schema the desired-state annotation is written
// under, and the only one this build will read back.
//
// UnmarshalIntent is a plain json.Unmarshal, which is silent in both
// directions: a field added to unleash.Config since an annotation was written
// comes back as its zero value, and a renamed one is dropped as unknown and
// zero-filled as missing. Because the annotation is authoritative, the
// reconciler would then render those zero values onto every live instance still
// carrying an old annotation — a fleet-wide rewrite that looks exactly like an
// ordinary converge. The version turns that into a refusal.
//
// Bump it in the same commit as any change to unleash.Config's fields — add,
// remove, rename, or retype. TestIntentSchemaVersionPinsConfigFields fails
// until you do.
const IntentSchemaVersion = 1

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
	b, err := json.Marshal(intentEnvelope{SchemaVersion: IntentSchemaVersion, Config: *cfg})
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
	if err := json.Unmarshal([]byte(s), &envelope); err != nil {
		return nil, err
	}

	version := envelope.SchemaVersion
	if version == 0 {
		// Written before the version field existed. Those annotations are
		// field-for-field what version 1 writes, so reading them as 1 is exact
		// rather than a guess — and it is deliberately not a blanket exemption:
		// once the schema moves past 1 they are refused along with every other
		// version-1 annotation.
		version = 1
	}
	if version != IntentSchemaVersion {
		return nil, fmt.Errorf("desired-state annotation has schema version %d, this build reads %d", version, IntentSchemaVersion)
	}

	cfg := envelope.Config
	return &cfg, nil
}

// IsManagedByBifrost reports whether the reconciler owns this instance.
func IsManagedByBifrost(crd *unleashv1.Unleash) bool {
	return crd.GetLabels()[LabelManagedBy] == ManagedByBifrost
}

// ApplyManagedMetadata copies bifrost's managed-by label and desired-state
// annotation from a rendered CRD onto a live one, leaving every other metadata
// field alone — finalizers, ownerReferences, and labels and annotations set by
// anyone else.
//
// This is the ownership rule for metadata on an Unleash CR: bifrost owns these
// two keys and nothing else. Both write paths into the object have to agree on
// it, or the outcome depends on which one wrote last — the reconciler's
// applyManagedMetadata has the same semantics, and the repository's Update
// reaches it through here.
func ApplyManagedMetadata(live, rendered *unleashv1.Unleash) {
	if live.Labels == nil {
		live.Labels = map[string]string{}
	}
	live.Labels[LabelManagedBy] = ManagedByBifrost

	// Absent only when marshaling the intent failed; keeping whatever the live
	// object already carries beats replacing it with nothing.
	if intent := rendered.GetAnnotations()[AnnotationDesiredState]; intent != "" {
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[AnnotationDesiredState] = intent
	}
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
