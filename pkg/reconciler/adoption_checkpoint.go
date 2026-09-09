package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	adoptionCheckpointName = "bifrost-legacy-adoption"
	adoptionCheckpointKey  = "checkpoint.json"
	maxConfigMapDataBytes  = 900 * 1024
)

type adoptionCheckpointPhase string

const (
	adoptionPrepared           adoptionCheckpointPhase = "prepared"
	adoptionTargetWritten      adoptionCheckpointPhase = "target-written"
	adoptionCheckpointVerified adoptionCheckpointPhase = "verified"
	adoptionFailed             adoptionCheckpointPhase = "manual-recovery-required"
)

// adoptionCheckpoint is the durable, single-flight adoption transaction. The
// source snapshot is kept in a ConfigMap rather than the CR annotation so it
// remains recoverable without consuming the annotation size budget.
type adoptionCheckpoint struct {
	SchemaVersion      int                     `json:"schemaVersion"`
	ID                 string                  `json:"id"`
	Phase              adoptionCheckpointPhase `json:"phase"`
	Namespace          string                  `json:"namespace"`
	ResourceName       string                  `json:"resourceName"`
	ResourceUID        types.UID               `json:"resourceUid"`
	SourceGeneration   int64                   `json:"sourceGeneration"`
	SourceSpec         json.RawMessage         `json:"sourceSpec"`
	SourceSpecSHA256   string                  `json:"sourceSpecSha256"`
	TargetSpecSHA256   string                  `json:"targetSpecSha256"`
	TargetIntentSHA256 string                  `json:"targetIntentSha256"`
	CreatedAt          metav1.Time             `json:"createdAt"`
	VerifiedAt         *metav1.Time            `json:"verifiedAt,omitempty"`
	Failure            string                  `json:"failure,omitempty"`
}

const adoptionCheckpointSchemaVersion = 1

func newAdoptionCheckpoint(plan *adoptionPlan, namespace, name string, resourceUID types.UID, generation int64) *adoptionCheckpoint {
	return &adoptionCheckpoint{
		SchemaVersion:      adoptionCheckpointSchemaVersion,
		ID:                 string(uuid.NewUUID()),
		Phase:              adoptionPrepared,
		Namespace:          namespace,
		ResourceName:       name,
		ResourceUID:        resourceUID,
		SourceGeneration:   generation,
		SourceSpec:         append(json.RawMessage(nil), plan.sourceSpec...),
		SourceSpecSHA256:   plan.sourceSpecSHA256,
		TargetSpecSHA256:   plan.targetSpecSHA256,
		TargetIntentSHA256: plan.targetIntentSHA,
		CreatedAt:          metav1.NewTime(time.Now().UTC()),
	}
}

func (checkpoint *adoptionCheckpoint) validate() error {
	if checkpoint.SchemaVersion != adoptionCheckpointSchemaVersion || checkpoint.ID == "" ||
		(checkpoint.Phase != adoptionPrepared && checkpoint.Phase != adoptionTargetWritten &&
			checkpoint.Phase != adoptionCheckpointVerified && checkpoint.Phase != adoptionFailed) ||
		checkpoint.Namespace == "" || checkpoint.ResourceName == "" || checkpoint.ResourceUID == "" ||
		checkpoint.SourceGeneration < 1 || checkpoint.CreatedAt.IsZero() ||
		!validSHA256(checkpoint.SourceSpecSHA256) || !validSHA256(checkpoint.TargetSpecSHA256) ||
		!validSHA256(checkpoint.TargetIntentSHA256) || len(checkpoint.SourceSpec) == 0 {
		return fmt.Errorf("invalid adoption checkpoint")
	}
	if kubernetes.HashBytes(checkpoint.SourceSpec) != checkpoint.SourceSpecSHA256 {
		return fmt.Errorf("adoption checkpoint source snapshot hash does not match")
	}
	var source any
	if err := json.Unmarshal(checkpoint.SourceSpec, &source); err != nil {
		return fmt.Errorf("decode adoption checkpoint source snapshot: %w", err)
	}
	if checkpoint.Phase == adoptionFailed && checkpoint.Failure == "" {
		return fmt.Errorf("failed adoption checkpoint has no actionable failure")
	}
	if checkpoint.Phase == adoptionCheckpointVerified && checkpoint.VerifiedAt == nil {
		return fmt.Errorf("verified adoption checkpoint has no verification time")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func marshalAdoptionCheckpoint(checkpoint *adoptionCheckpoint) (string, error) {
	if err := checkpoint.validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return "", fmt.Errorf("marshal adoption checkpoint: %w", err)
	}
	if len(raw) > maxConfigMapDataBytes {
		return "", fmt.Errorf("adoption checkpoint source snapshot exceeds ConfigMap data limit")
	}
	return string(raw), nil
}

func unmarshalAdoptionCheckpoint(raw string) (*adoptionCheckpoint, error) {
	checkpoint := &adoptionCheckpoint{}
	if err := json.Unmarshal([]byte(raw), checkpoint); err != nil {
		return nil, fmt.Errorf("decode adoption checkpoint: %w", err)
	}
	if err := checkpoint.validate(); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func checkpointConfigMap(namespace string, checkpoint *adoptionCheckpoint) (*corev1.ConfigMap, error) {
	raw, err := marshalAdoptionCheckpoint(checkpoint)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      adoptionCheckpointName,
			Namespace: namespace,
			Labels: map[string]string{
				kubernetes.LabelManagedBy: kubernetes.ManagedByBifrost,
			},
		},
		Data: map[string]string{adoptionCheckpointKey: raw},
	}, nil
}

func checkpointFromConfigMap(configMap *corev1.ConfigMap) (*adoptionCheckpoint, error) {
	raw := configMap.Data[adoptionCheckpointKey]
	if raw == "" {
		return nil, fmt.Errorf("adoption checkpoint ConfigMap %s has no %s", configMap.Name, adoptionCheckpointKey)
	}
	return unmarshalAdoptionCheckpoint(raw)
}

func (r *UnleashReconciler) getAdoptionCheckpoint(ctx context.Context, namespace string) (*corev1.ConfigMap, *adoptionCheckpoint, bool, error) {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: adoptionCheckpointName}
	if err := r.client.Get(ctx, key, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("get adoption checkpoint ConfigMap: %w", err)
	}
	checkpoint, err := checkpointFromConfigMap(configMap)
	if err != nil {
		return configMap, nil, true, err
	}
	if checkpoint.Namespace != namespace {
		return configMap, nil, true, fmt.Errorf("adoption checkpoint ConfigMap namespace does not match the configured instance namespace")
	}
	return configMap, checkpoint, true, nil
}

func (r *UnleashReconciler) createAdoptionCheckpoint(ctx context.Context, namespace string, checkpoint *adoptionCheckpoint) (*corev1.ConfigMap, error) {
	configMap, err := checkpointConfigMap(namespace, checkpoint)
	if err != nil {
		return nil, err
	}
	if err := r.client.Create(ctx, configMap); err != nil {
		return nil, fmt.Errorf("create adoption checkpoint ConfigMap: %w", err)
	}
	return configMap, nil
}

func (r *UnleashReconciler) updateAdoptionCheckpoint(ctx context.Context, configMap *corev1.ConfigMap, checkpoint *adoptionCheckpoint) error {
	raw, err := marshalAdoptionCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	base := configMap.DeepCopy()
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data[adoptionCheckpointKey] = raw
	if err := r.client.Patch(ctx, configMap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("update adoption checkpoint ConfigMap: %w", err)
	}
	return nil
}

func checkpointMatchesPlan(checkpoint *adoptionCheckpoint, plan *adoptionPlan, crdUID types.UID) bool {
	return checkpoint.ResourceUID == crdUID &&
		checkpoint.SourceSpecSHA256 == plan.sourceSpecSHA256 &&
		checkpoint.TargetSpecSHA256 == plan.targetSpecSHA256 &&
		checkpoint.TargetIntentSHA256 == plan.targetIntentSHA
}

func checkpointMatchesRecord(checkpoint *adoptionCheckpoint, configMap *corev1.ConfigMap, record kubernetes.AdoptionRecord) error {
	if record.CheckpointName != configMap.Name || record.CheckpointID != checkpoint.ID ||
		record.ResourceUID != checkpoint.ResourceUID ||
		record.SourceGeneration != checkpoint.SourceGeneration ||
		record.SourceSpecSHA256 != checkpoint.SourceSpecSHA256 ||
		record.TargetSpecSHA256 != checkpoint.TargetSpecSHA256 ||
		record.TargetIntentSHA256 != checkpoint.TargetIntentSHA256 {
		return fmt.Errorf("CR adoption record does not match ConfigMap checkpoint")
	}
	if configMap.UID != "" && record.CheckpointConfigMapUID != configMap.UID {
		return fmt.Errorf("adoption checkpoint ConfigMap was replaced or the CR record is stale")
	}
	return nil
}
