package unleash

import (
	"time"

	unleashv1 "github.com/nais/unleasherator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UnleashInstance represents the old format for backward compatibility with v0 API
type UnleashInstance struct {
	Name                string
	KubernetesNamespace string
	CreatedAt           metav1.Time
	ServerInstance      *unleashv1.Unleash
}

// NewUnleashInstance creates a new UnleashInstance from an Unleash CRD
func NewUnleashInstance(serverInstance *unleashv1.Unleash) *UnleashInstance {
	return &UnleashInstance{
		Name:                serverInstance.Name,
		KubernetesNamespace: serverInstance.Namespace,
		CreatedAt:           serverInstance.CreationTimestamp,
		ServerInstance:      serverInstance,
	}
}

// Age returns the age of the instance as a human-readable string
func (u *UnleashInstance) Age() string {
	duration := time.Since(u.CreatedAt.Time)

	days := int(duration.Hours() / 24)
	weeks := days / 7
	months := days / 30
	years := days / 365

	switch {
	case years >= 2:
		return "2 years"
	case years >= 1:
		return "1 year"
	case months >= 2:
		return "2 months"
	case months >= 1:
		return "1 month"
	case weeks >= 2:
		return "2 weeks"
	case weeks >= 1:
		return "1 week"
	case days >= 2:
		return "2 days"
	case days >= 1:
		return "1 day"
	default:
		return "less than a day"
	}
}

// WebUrl returns the web URL for the instance
func (u *UnleashInstance) WebUrl() string {
	return "https://" + u.ServerInstance.Spec.WebIngress.Host + "/"
}

// ApiUrl returns the API URL for the instance
func (u *UnleashInstance) ApiUrl() string {
	return "https://" + u.ServerInstance.Spec.ApiIngress.Host + "/api/"
}

// IsReady returns true if the instance is ready
func (u *UnleashInstance) IsReady() bool {
	if u.ServerInstance == nil {
		return false
	}

	reconciledOK := false
	connectedOK := false

	for _, condition := range u.ServerInstance.Status.Conditions {
		if condition.Type == unleashv1.UnleashStatusConditionTypeReconciled && condition.Status == metav1.ConditionTrue {
			reconciledOK = true
		}
		if condition.Type == unleashv1.UnleashStatusConditionTypeConnected && condition.Status == metav1.ConditionTrue {
			connectedOK = true
		}
	}

	return reconciledOK && connectedOK
}

// Status returns the status of the instance
func (u *UnleashInstance) Status() string {
	if u.ServerInstance == nil {
		return "Status unknown"
	}

	if u.IsReady() {
		return "Ready"
	}

	// Check if reconciled but not connected
	for _, condition := range u.ServerInstance.Status.Conditions {
		if condition.Type == unleashv1.UnleashStatusConditionTypeReconciled && condition.Status == metav1.ConditionFalse {
			return "Not ready"
		}
	}

	return "Pending"
}

// StatusLabel returns a color label for the instance status
func (u *UnleashInstance) StatusLabel() string {
	if u.ServerInstance == nil {
		return "orange"
	}

	// Check if both Reconciled and Connected conditions are True
	reconciledOK := false
	connectedOK := false

	for _, condition := range u.ServerInstance.Status.Conditions {
		if condition.Type == unleashv1.UnleashStatusConditionTypeReconciled && condition.Status == metav1.ConditionTrue {
			reconciledOK = true
		}
		if condition.Type == unleashv1.UnleashStatusConditionTypeConnected && condition.Status == metav1.ConditionTrue {
			connectedOK = true
		}
	}

	if reconciledOK && connectedOK {
		return "green"
	}
	return "red"
}
