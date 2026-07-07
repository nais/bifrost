package kubernetes

import (
	"encoding/json"

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
	// unleash.Config) as JSON. It is the authoritative, non-lossy source of
	// truth the reconciler re-renders from — unlike reverse-engineering the
	// rendered spec via LoadConfigFromCRD, which drops fields.
	AnnotationDesiredState = "bifrost.nais.io/desired-state"
)

// MarshalIntent serializes a per-instance config for storage in the
// desired-state annotation.
func MarshalIntent(cfg *unleash.Config) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalIntent parses the desired-state annotation back into a config.
func UnmarshalIntent(s string) (*unleash.Config, error) {
	cfg := &unleash.Config{}
	if err := json.Unmarshal([]byte(s), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// IsManagedByBifrost reports whether the reconciler owns this instance.
func IsManagedByBifrost(crd *unleashv1.Unleash) bool {
	return crd.GetLabels()[LabelManagedBy] == ManagedByBifrost
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
