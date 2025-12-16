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

	// Image is the full container image from spec.image (e.g., "quay.io/unleash/unleash-server:6.3.0")
	Image string

	// CreatedAt is when the channel was created
	CreatedAt time.Time

	// Spec contains the channel specification
	Spec ChannelSpec

	// Status contains the channel's current state
	Status ChannelStatus
}

// ChannelSpec defines the desired state of a ReleaseChannel
type ChannelSpec struct {
	// MaxParallel is the maximum number of instances to deploy in parallel
	MaxParallel int

	// CanaryEnabled indicates if canary deployment is enabled
	CanaryEnabled bool
}

// ChannelStatus defines the observed state of a ReleaseChannel
type ChannelStatus struct {
	// Phase is the current rollout phase (Idle, Canary, Rolling, Completed, Failed, etc.)
	Phase string

	// Instances is the total number of instances managed by this channel
	Instances int

	// InstancesUpToDate is the number of instances running the target image
	InstancesUpToDate int

	// Progress is the rollout progress as a percentage (0-100)
	Progress int

	// Completed indicates if the rollout is complete
	Completed bool

	// FailureReason provides the reason for failure if the rollout failed
	FailureReason string

	// LastReconcileTime is when the channel was last reconciled
	LastReconcileTime metav1.Time

	// Conditions represent the channel's operational state
	Conditions []metav1.Condition
}

// Age returns how long ago the channel was created
func (c *Channel) Age() time.Duration {
	return time.Since(c.CreatedAt)
}

// IsCanary returns true if the channel has canary deployment enabled
func (c *Channel) IsCanary() bool {
	return c.Spec.CanaryEnabled
}
