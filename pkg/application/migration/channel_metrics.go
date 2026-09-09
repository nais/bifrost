package migration

import "github.com/prometheus/client_golang/prometheus"

const (
	channelEventAdmitted           = "admitted"
	channelEventResumed            = "resumed"
	channelEventCompleted          = "completed"
	channelEventRolledBack         = "rolled_back"
	channelEventManualRecovery     = "manual_recovery"
	channelEventConflictRetry      = "conflict_retry"
	channelEventSkippedUnmanaged   = "skipped_unmanaged"
	channelEventSkippedNoIntent    = "skipped_invalid_intent"
	channelEventSkippedUnhealthy   = "skipped_unhealthy"
	channelEventSkippedAdoption    = "skipped_pending_adoption"
	channelEventCanaryLimited      = "canary_limited"
	channelEventOwnershipChanged   = "ownership_changed"
	channelEventInvalidTransaction = "invalid_transaction"
)

var channelMigrationEvents = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "bifrost_channel_migration_events_total",
		Help: "Channel migration state-machine events. The event label has a fixed, bounded set and never identifies an instance.",
	},
	[]string{"event"},
)

func init() {
	prometheus.MustRegister(channelMigrationEvents)
}

func recordChannelMigrationEvent(event string) {
	channelMigrationEvents.WithLabelValues(event).Inc()
}
