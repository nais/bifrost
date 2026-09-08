package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	channelMigrationAnnotation    = kubernetes.AnnotationChannelMigration
	channelTransactionSchema      = 1
	maxTransactionFailureReason   = 120
	targetWriteRetries            = 3
	transactionAnnotationRetries  = 3
	defaultTransactionRetryDelay  = 50 * time.Millisecond
	failureTargetWrite            = "target_write_failed"
	failureTargetTimeout          = "target_health_timeout"
	failureTargetChannelChanged   = "target_channel_changed"
	failureSourceUnhealthy        = "source_unhealthy"
	failureRollbackWrite          = "rollback_write_failed"
	failureRollbackTimeout        = "rollback_health_timeout"
	failureTransactionDeadline    = "transaction_deadline_exceeded"
	failureIntentOwnershipChanged = "desired_state_ownership_changed"
)

type channelMigrationPhase string

const (
	phasePrepared        channelMigrationPhase = "prepared"
	phaseTargetWritten   channelMigrationPhase = "target-written"
	phaseRollbackPending channelMigrationPhase = "rollback-pending"
	phaseRollbackWritten channelMigrationPhase = "rollback-written"
	phaseCompleted       channelMigrationPhase = "completed"
	phaseRolledBack      channelMigrationPhase = "rolled-back"
	phaseManualRecovery  channelMigrationPhase = "manual-recovery-required"
)

var (
	errTargetChannelChanged = errors.New("pinned target channel changed")
	errTransactionTimeout   = errors.New("channel migration transaction deadline exceeded")
)

type channelMigrationTransaction struct {
	SchemaVersion                int                   `json:"schemaVersion"`
	ResourceUID                  types.UID             `json:"resourceUid"`
	Phase                        channelMigrationPhase `json:"phase"`
	Deadline                     time.Time             `json:"deadline"`
	SourceChannel                string                `json:"sourceChannel"`
	TargetChannel                string                `json:"targetChannel"`
	TargetChannelUID             types.UID             `json:"targetChannelUid"`
	TargetImage                  string                `json:"targetImage"`
	DesiredStateIntentHash       string                `json:"desiredStateIntentHash"`
	TargetDesiredStateIntentHash string                `json:"targetDesiredStateIntentHash"`
	FailureReason                string                `json:"failureReason,omitempty"`
}

func (t *channelMigrationTransaction) validate() error {
	if t.SchemaVersion != channelTransactionSchema {
		return fmt.Errorf("channel migration transaction has schema version %d, this build reads %d", t.SchemaVersion, channelTransactionSchema)
	}
	if t.ResourceUID == "" {
		return errors.New("channel migration transaction has no resource UID")
	}
	if t.Deadline.IsZero() {
		return errors.New("channel migration transaction has no deadline")
	}
	if t.SourceChannel == "" || t.TargetChannel == "" || t.SourceChannel == t.TargetChannel {
		return errors.New("channel migration transaction has invalid source or target channel")
	}
	if t.TargetChannelUID == "" || t.TargetImage == "" {
		return errors.New("channel migration transaction has no pinned target channel UID or image")
	}
	if t.DesiredStateIntentHash == "" || t.TargetDesiredStateIntentHash == "" {
		return errors.New("channel migration transaction has no desired-state intent hash")
	}
	switch t.Phase {
	case phasePrepared, phaseTargetWritten, phaseRollbackPending, phaseRollbackWritten,
		phaseCompleted, phaseRolledBack, phaseManualRecovery:
	default:
		return fmt.Errorf("channel migration transaction has unknown phase %q", t.Phase)
	}
	if len(t.FailureReason) > maxTransactionFailureReason {
		return errors.New("channel migration transaction failure reason is too long")
	}
	return nil
}

func marshalChannelMigrationTransaction(transaction *channelMigrationTransaction) (string, error) {
	if err := transaction.validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(transaction)
	if err != nil {
		return "", fmt.Errorf("marshal channel migration transaction: %w", err)
	}
	return string(raw), nil
}

func unmarshalChannelMigrationTransaction(raw string) (*channelMigrationTransaction, error) {
	transaction := &channelMigrationTransaction{}
	if err := json.Unmarshal([]byte(raw), transaction); err != nil {
		return nil, fmt.Errorf("unmarshal channel migration transaction: %w", err)
	}
	if err := transaction.validate(); err != nil {
		return nil, err
	}
	return transaction, nil
}

func loadDesiredStateIntent(crd *unleashv1.Unleash) (*unleash.Config, string, error) {
	raw := crd.GetAnnotations()[kubernetes.AnnotationDesiredState]
	if raw == "" {
		return nil, "", errors.New("desired-state annotation is missing")
	}

	cfg, err := kubernetes.UnmarshalIntent(raw)
	if err != nil {
		return nil, "", fmt.Errorf("desired-state annotation is invalid: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, "", fmt.Errorf("desired-state annotation is not a valid config: %w", err)
	}
	if cfg.Name != crd.GetName() {
		return nil, "", fmt.Errorf("desired-state annotation names %q but is on instance %q", cfg.Name, crd.GetName())
	}

	hash, err := desiredStateIntentHash(cfg)
	if err != nil {
		return nil, "", err
	}
	return cfg, hash, nil
}

func desiredStateIntentHash(cfg *unleash.Config) (string, error) {
	raw, err := kubernetes.MarshalIntent(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal desired-state intent for hashing: %w", err)
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), nil
}

func targetIntent(source *unleash.Config, targetChannel string) (*unleash.Config, string, error) {
	target := *source
	target.CustomVersion = ""
	target.ReleaseChannelName = targetChannel
	if err := target.Validate(); err != nil {
		return nil, "", fmt.Errorf("target desired-state intent is invalid: %w", err)
	}
	hash, err := desiredStateIntentHash(&target)
	if err != nil {
		return nil, "", err
	}
	return &target, hash, nil
}

type transactionOwnershipError struct {
	reason string
}

func (e *transactionOwnershipError) Error() string {
	return e.reason
}

func ownershipError(reason string) error {
	return &transactionOwnershipError{reason: reason}
}

func isOwnershipError(err error) bool {
	var target *transactionOwnershipError
	return errors.As(err, &target)
}
