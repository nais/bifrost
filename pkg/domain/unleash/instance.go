package unleash

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Instance represents an Unleash feature flag server instance
type Instance struct {
	Name      string
	Namespace string
	CreatedAt time.Time
	Version   string
	IsReady   bool
	APIUrl    string
	WebUrl    string

	// Version source configuration
	CustomVersion      string
	ReleaseChannelName string

	// Read-only status from CRD
	ResolvedImage         string
	ChannelNameFromStatus string
}

// HasReleaseChannel returns true if this instance is configured with a release channel
func (i *Instance) HasReleaseChannel() bool {
	return i.ReleaseChannelName != ""
}

// Age returns a human-readable string representing how long ago the instance was created
func (i *Instance) Age() string {
	duration := time.Since(i.CreatedAt)

	switch {
	case duration < 24*time.Hour:
		return "less than a day"
	case duration < 2*24*time.Hour:
		return "1 day"
	case duration < 7*24*time.Hour:
		return fmt.Sprintf("%d days", int(duration.Hours()/24))
	case duration < 14*24*time.Hour:
		return "1 week"
	case duration < 30*24*time.Hour:
		return fmt.Sprintf("%d weeks", int(duration.Hours()/24/7))
	case duration < 60*24*time.Hour:
		return "1 month"
	case duration < 365*24*time.Hour:
		return fmt.Sprintf("%d months", int(duration.Hours()/24/30))
	case duration < 2*365*24*time.Hour:
		return "1 year"
	default:
		return fmt.Sprintf("%d years", int(duration.Hours()/24/365))
	}
}

// Status returns a human-readable status string
func (i *Instance) Status() string {
	if i.IsReady {
		return "Ready"
	}
	return "Not ready"
}

// StatusLabel returns a color label for UI purposes
func (i *Instance) StatusLabel() string {
	if i.IsReady {
		return "green"
	}
	return "red"
}

// VersionSource returns the source of the version configuration
func (i *Instance) VersionSource() string {
	if i.CustomVersion != "" {
		return "custom"
	}
	if i.ReleaseChannelName != "" {
		return "releaseChannel"
	}
	return "default"
}

// NewInstance creates a new Instance from basic parameters
func NewInstance(name, namespace string, createdAt metav1.Time) *Instance {
	return &Instance{
		Name:      name,
		Namespace: namespace,
		CreatedAt: createdAt.Time,
	}
}
