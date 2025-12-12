package releasechannel

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Repository defines the interface for ReleaseChannel persistence operations
type Repository interface {
	// List retrieves all ReleaseChannel resources
	List(ctx context.Context) ([]*Channel, error)

	// Get retrieves a single ReleaseChannel by name
	Get(ctx context.Context, name string) (*Channel, error)
}

// Channel represents a ReleaseChannel CRD from unleasherator
// ReleaseChannels define available Unleash version channels that instances can subscribe to
type Channel struct {
	// Name is the channel identifier (e.g., "stable", "rapid", "regular")
	Name string

	// Version is the current Unleash version on this channel
	Version string

	// CreatedAt is when the channel was created
	CreatedAt time.Time

	// Spec contains the raw channel specification
	Spec ChannelSpec

	// Status contains the channel's current state
	Status ChannelStatus
}

// ChannelSpec defines the desired state of a ReleaseChannel
type ChannelSpec struct {
	// Type is the channel type (e.g., "automatic", "manual")
	Type string

	// Schedule is the update schedule (cron expression) if automatic
	Schedule string

	// Description provides information about the channel's purpose
	Description string
}

// ChannelStatus defines the observed state of a ReleaseChannel
type ChannelStatus struct {
	// CurrentVersion is the current Unleash version on this channel
	CurrentVersion string

	// LastUpdated is when the channel version was last updated
	LastUpdated metav1.Time

	// Conditions represent the channel's operational state
	Conditions []metav1.Condition
}

// Age returns how long ago the channel was created
func (c *Channel) Age() time.Duration {
	return time.Since(c.CreatedAt)
}

// IsAutomatic returns true if the channel is automatically updated
func (c *Channel) IsAutomatic() bool {
	return c.Spec.Type == "automatic"
}
