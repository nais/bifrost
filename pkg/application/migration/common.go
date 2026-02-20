package migration

import (
	"context"
	"sync"
	"time"

	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/sirupsen/logrus"
)

type migrationStatus string

const (
	statusPending          migrationStatus = "pending"
	statusInProgress       migrationStatus = "in-progress"
	statusCompleted        migrationStatus = "completed"
	statusFailed           migrationStatus = "failed"
	statusRolledBack       migrationStatus = "rolled-back"
	statusRollbackFailed   migrationStatus = "rollback-failed"
	statusSkippedUnhealthy migrationStatus = "skipped-unhealthy"

	defaultPollInterval = 10 * time.Second
)

// instanceState tracks migration state with the original value for rollback
type instanceState struct {
	originalValue string
	status        migrationStatus
}

// pendingQueue manages the ordered list of instances pending migration
type pendingQueue struct {
	mu      sync.Mutex
	pending []string
}

func newPendingQueue() *pendingQueue {
	return &pendingQueue{
		pending: make([]string, 0),
	}
}

func (q *pendingQueue) add(names ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, names...)
}

func (q *pendingQueue) remove(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, n := range q.pending {
		if n == name {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

func (q *pendingQueue) copy() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	result := make([]string, len(q.pending))
	copy(result, q.pending)
	return result
}

// migrationSummary aggregates status counts from migration state
type migrationSummary struct {
	Completed      int
	Failed         int
	Skipped        int
	RolledBack     int
	RollbackFailed int
}

// computeSummary iterates over state map and counts statuses
func computeSummary(state *sync.Map) migrationSummary {
	var s migrationSummary

	state.Range(func(_, value any) bool {
		st := value.(*instanceState)
		switch st.status {
		case statusCompleted:
			s.Completed++
		case statusFailed:
			s.Failed++
		case statusSkippedUnhealthy:
			s.Skipped++
		case statusRolledBack:
			s.RolledBack++
		case statusRollbackFailed:
			s.RollbackFailed++
		}
		return true
	})

	return s
}

// logSummary logs the migration summary with appropriate severity
func logSummary(logger *logrus.Logger, summary migrationSummary, reconcilerName string) {
	log := logger.WithFields(logrus.Fields{
		"completed":       summary.Completed,
		"failed":          summary.Failed,
		"skipped":         summary.Skipped,
		"rolled_back":     summary.RolledBack,
		"rollback_failed": summary.RollbackFailed,
	})

	if summary.RollbackFailed > 0 {
		log.Errorf("%s completed with rollback failures - manual intervention required", reconcilerName)
	} else if summary.Failed > 0 || summary.RolledBack > 0 {
		log.Warnf("%s completed with issues", reconcilerName)
	} else {
		log.Infof("%s completed successfully", reconcilerName)
	}
}

// logCurrentState logs the current migration state for debugging/operator visibility
func logCurrentState(logger *logrus.Logger, state *sync.Map, pending *pendingQueue, reason string) {
	pendingCopy := pending.copy()

	states := make(map[string]string)
	state.Range(func(key, value any) bool {
		name := key.(string)
		st := value.(*instanceState)
		states[name] = string(st.status)
		return true
	})

	logger.WithFields(logrus.Fields{
		"reason":  reason,
		"pending": pendingCopy,
		"states":  states,
	}).Info("Migration reconciler state")
}

// waitForHealthy polls the instance until it becomes ready or timeout expires
func waitForHealthy(ctx context.Context, repo unleash.Repository, logger *logrus.Logger, name string, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return &healthCheckTimeoutError{name: name, timeout: timeout}
			}

			inst, err := repo.Get(ctx, name)
			if err != nil {
				logger.WithError(err).WithField("instance", name).Debug("Health check failed to get instance")
				continue
			}

			if inst.IsReady {
				return nil
			}

			logger.WithFields(logrus.Fields{
				"instance":  name,
				"isReady":   inst.IsReady,
				"remaining": time.Until(deadline).Round(time.Second),
			}).Debug("Waiting for instance to become healthy")
		}
	}
}

// healthCheckTimeoutError is returned when health check times out
type healthCheckTimeoutError struct {
	name    string
	timeout time.Duration
}

func (e *healthCheckTimeoutError) Error() string {
	return "health check timed out for instance " + e.name + " after " + e.timeout.String()
}
