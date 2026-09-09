package reconciler

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// adoptionPlan is a pure description of one legacy CR's canonicalization. It
// has no client dependency: callers can inspect it in dry-run without creating
// a checkpoint or modifying the CR.
type adoptionPlan struct {
	sourceSpec       json.RawMessage
	sourceSpecSHA256 string
	target           unleashv1.Unleash
	targetSpecSHA256 string
	targetIntent     string
	targetIntentSHA  string
	canonicalized    []string
}

var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// planLegacyAdoption establishes the narrow contract full adoption supports.
// It only normalizes Bifrost-owned fields. Additions Bifrost cannot account for
// (extra volumes, containers, pod metadata or unsupported environment) are
// rejected instead of being silently deleted.
func planLegacyAdoption(crd *unleashv1.Unleash, cfg *config.Config) (*adoptionPlan, error) {
	if err := validateLegacyShape(crd); err != nil {
		return nil, err
	}

	legacyConfig, err := kubernetes.LoadConfigFromCRD(crd).Build()
	if err != nil {
		return nil, fmt.Errorf("derive Bifrost-supported fields: %w", err)
	}

	if crd.Spec.CustomImage != "" {
		tag, err := legacyImageTag(crd.Spec.CustomImage)
		if err != nil {
			return nil, err
		}
		legacyConfig.CustomVersion = tag
		legacyConfig.ReleaseChannelName = ""
	}
	// BuildUnleashCRD must never generate a nonce while planning. The
	// compatibility check above refuses an absent nonce, so preserving the
	// source value makes repeated plans byte-for-byte stable.
	legacyConfig.FederationNonce = crd.Spec.Federation.SecretNonce

	target := kubernetes.BuildUnleashCRD(cfg, legacyConfig)
	if err := validateCanonicalizationContract(crd, &target); err != nil {
		return nil, err
	}

	sourceSpec, err := json.Marshal(crd.Spec)
	if err != nil {
		return nil, fmt.Errorf("snapshot source spec: %w", err)
	}
	sourceHash := kubernetes.HashBytes(sourceSpec)
	targetHash, err := kubernetes.HashJSON(target.Spec)
	if err != nil {
		return nil, fmt.Errorf("hash canonical target spec: %w", err)
	}
	intent := target.GetAnnotations()[kubernetes.AnnotationDesiredState]
	if intent == "" {
		return nil, fmt.Errorf("render canonical desired-state intent")
	}

	return &adoptionPlan{
		sourceSpec:       sourceSpec,
		sourceSpecSHA256: sourceHash,
		target:           target,
		targetSpecSHA256: targetHash,
		targetIntent:     intent,
		targetIntentSHA:  kubernetes.HashBytes([]byte(intent)),
		canonicalized:    canonicalizedSpecFields(crd, &target),
	}, nil
}

func validateLegacyShape(crd *unleashv1.Unleash) error {
	spec := crd.Spec
	if spec.CustomImage != "" && spec.ReleaseChannel.Name != "" {
		return refuseAdoption("spec.customImage and spec.releaseChannel are both set; choose one version source before opting in")
	}
	if spec.CustomImage == "" && spec.ReleaseChannel.Name == "" {
		return refuseAdoption("spec has no customImage or releaseChannel; add an explicit Bifrost-supported version source before opting in")
	}
	if spec.Federation.SecretNonce == "" {
		return refuseAdoption("spec.federation.secretNonce is empty; adoption will not generate a new federation secret")
	}
	if !spec.Federation.Enabled && (len(spec.Federation.Namespaces) != 0 || len(spec.Federation.Clusters) != 0) {
		return refuseAdoption("spec.federation has namespaces or clusters while disabled; correct the manual shape before opting in")
	}
	if len(spec.ExtraVolumes) != 0 || len(spec.ExtraVolumeMounts) != 0 {
		return refuseAdoption("spec.extraVolumes or spec.extraVolumeMounts is set; Bifrost does not own these fields")
	}
	if len(spec.PodAnnotations) != 0 || len(spec.PodLabels) != 0 {
		return refuseAdoption("spec.podAnnotations or spec.podLabels is set; Bifrost does not own pod metadata")
	}
	if len(spec.ExtraContainers) > 1 ||
		(len(spec.ExtraContainers) == 1 && spec.ExtraContainers[0].Name != "sql-proxy") {
		return refuseAdoption("spec.extraContainers contains a container other than Bifrost's sql-proxy")
	}

	seenEnv := make(map[string]struct{}, len(spec.ExtraEnvVars))
	for _, env := range spec.ExtraEnvVars {
		if env.ValueFrom != nil {
			return refuseAdoption("spec.extraEnvVars contains valueFrom; Bifrost only owns literal canonical environment variables")
		}
		if _, found := supportedLegacyEnv[env.Name]; !found {
			return refuseAdoption(fmt.Sprintf("spec.extraEnvVars contains unsupported variable %q; remove it or manage this CR manually", env.Name))
		}
		if _, duplicate := seenEnv[env.Name]; duplicate {
			return refuseAdoption(fmt.Sprintf("spec.extraEnvVars contains duplicate variable %q; make the source unambiguous before opting in", env.Name))
		}
		seenEnv[env.Name] = struct{}{}
	}
	return nil
}

var supportedLegacyEnv = map[string]struct{}{
	"OAUTH_JWT_AUDIENCE":            {},
	"OAUTH_JWT_AUTH":                {},
	"NAIS_API_ADDRESS":              {},
	"TEAMS_ALLOWED_TEAMS":           {},
	"LOG_LEVEL":                     {},
	"DATABASE_POOL_MAX":             {},
	"DATABASE_POOL_IDLE_TIMEOUT_MS": {},
}

// validateCanonicalizationContract checks every source field whose value may
// differ from Bifrost's render. The allowed differences are intentional:
// version source, replica count, metrics, ingress endpoints, federation,
// Bifrost's seven literal env vars, and its sql-proxy sidecar. Database
// identity, network policy, resources and service account are exact safety
// boundaries and must already match before adoption.
func validateCanonicalizationContract(source, target *unleashv1.Unleash) error {
	if !equality.Semantic.DeepEqual(source.Spec.Database, target.Spec.Database) {
		return refuseAdoption("spec.database differs from Bifrost's canonical database identity; do not auto-adopt a CR connected to an unknown database")
	}
	if !equality.Semantic.DeepEqual(source.Spec.NetworkPolicy, target.Spec.NetworkPolicy) {
		return refuseAdoption("spec.networkPolicy differs from Bifrost's canonical policy; review the manual network rules before opting in")
	}
	if source.Spec.ExistingServiceAccountName != target.Spec.ExistingServiceAccountName {
		return refuseAdoption("spec.existingServiceAccountName differs from Bifrost's configured service account")
	}
	if !equality.Semantic.DeepEqual(source.Spec.Resources, target.Spec.Resources) {
		return refuseAdoption("spec.resources differs from Bifrost's canonical resources; review the manual workload sizing before opting in")
	}
	if source.Spec.WebIngress.TLS != nil || len(source.Spec.WebIngress.Annotations) != 0 ||
		source.Spec.ApiIngress.TLS != nil || len(source.Spec.ApiIngress.Annotations) != 0 {
		return refuseAdoption("spec ingress TLS or annotations are set; Bifrost does not own manual ingress extensions")
	}

	knownFields := map[string]struct{}{
		"Size": {}, "CustomImage": {}, "ReleaseChannel": {}, "Prometheus": {},
		"Database": {}, "WebIngress": {}, "ApiIngress": {}, "NetworkPolicy": {},
		"ExtraEnvVars": {}, "ExtraVolumes": {}, "ExtraVolumeMounts": {},
		"ExtraContainers": {}, "ExistingServiceAccountName": {}, "Resources": {},
		"Federation": {}, "PodAnnotations": {}, "PodLabels": {},
	}
	sourceValue := reflect.ValueOf(source.Spec)
	targetValue := reflect.ValueOf(target.Spec)
	specType := sourceValue.Type()
	for i := range sourceValue.NumField() {
		field := specType.Field(i)
		if _, known := knownFields[field.Name]; !known &&
			!equality.Semantic.DeepEqual(sourceValue.Field(i).Interface(), targetValue.Field(i).Interface()) {
			return refuseAdoption(fmt.Sprintf("spec.%s has no Bifrost canonicalization contract; upgrade Bifrost or manage this CR manually", field.Name))
		}
	}
	return nil
}

func legacyImageTag(image string) (string, error) {
	if strings.TrimSpace(image) != image {
		return "", refuseAdoption("spec.customImage has leading or trailing whitespace")
	}
	if strings.Contains(image, "@") {
		return "", refuseAdoption("spec.customImage uses a digest; Bifrost only supports a tagged legacy custom image")
	}
	lastSlash := strings.LastIndex(image, "/")
	separator := strings.LastIndex(image, ":")
	if separator <= lastSlash || separator == 0 || separator == len(image)-1 ||
		strings.Count(image[lastSlash+1:], ":") != 1 {
		return "", refuseAdoption("spec.customImage has no unambiguous image tag")
	}
	tag := image[separator+1:]
	if !imageTagPattern.MatchString(tag) {
		return "", refuseAdoption("spec.customImage has an unsupported image tag")
	}
	return tag, nil
}

func canonicalizedSpecFields(source, target *unleashv1.Unleash) []string {
	sourceValue := reflect.ValueOf(source.Spec)
	targetValue := reflect.ValueOf(target.Spec)
	specType := sourceValue.Type()
	fields := make([]string, 0, sourceValue.NumField())
	for i := range sourceValue.NumField() {
		if !equality.Semantic.DeepEqual(sourceValue.Field(i).Interface(), targetValue.Field(i).Interface()) {
			fields = append(fields, "spec."+specType.Field(i).Name)
		}
	}
	sort.Strings(fields)
	return fields
}

func refuseAdoption(reason string) error {
	return fmt.Errorf("refusing legacy adoption: %s", reason)
}
