package migration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileSchemaResumesLegacyInFlightTransaction(t *testing.T) {
	// Frozen schema-1 serialization: old binaries hashed this, not the annotation bytes.
	const legacy = `{"schemaVersion":1,"Name":"team-a","CustomVersion":"","ReleaseChannelName":"stable-v6","EnableFederation":false,"FederationNonce":"","AllowedTeams":"","AllowedNamespaces":"","AllowedClusters":"","LogLevel":"warn","DatabasePoolMax":3,"DatabasePoolIdleTimeoutMs":1000}`
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			repo := NewMockUnleashRepository()
			repo.AddInstance("team-a", "", "stable-v6", true)
			channels := channelTestChannels()
			installTransaction(t, repo, channels, "team-a", phasePrepared, time.Now().Add(time.Minute))
			tx := mustTransaction(t, repo, "team-a")
			targetRaw := strings.Replace(legacy, "stable-v6", "stable-v7", 1)
			tx.DesiredStateIntentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
			tx.TargetDesiredStateIntentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(targetRaw)))
			raw, err := marshalChannelMigrationTransaction(tx)
			require.NoError(t, err)
			setDesiredChannel(t, repo, "team-a", "stable-v7")
			if version == 0 {
				targetRaw = strings.Replace(targetRaw, `"schemaVersion":1,`, "", 1)
			}
			repo.mu.Lock()
			repo.crds["team-a"].Annotations[kubernetes.AnnotationDesiredState] = targetRaw
			repo.crds["team-a"].Annotations[channelMigrationAnnotation] = raw
			repo.mu.Unlock()
			repo.SetReadyOnChannel("team-a", "stable-v7", "unleash/unleash-server:7.6.5")
			cfg := newChannelTestConfig(false, "", time.Second)
			newChannelTestReconciler(repo, channels, cfg).Start(context.Background())
			completed := mustTransaction(t, repo, "team-a")
			assert.Equal(t, phaseCompleted, completed.Phase)
			assert.Equal(t, tx.Deadline, completed.Deadline, "resume the pinned transaction, do not restart it")
			assert.Equal(t, tx.TargetDesiredStateIntentHash, completed.TargetDesiredStateIntentHash)
			assert.Zero(t, repo.updateAttempts, "already-written target must be recognized under both old schemas")
		})
	}
}
