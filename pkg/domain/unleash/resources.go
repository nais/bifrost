package unleash

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ValidResourceProfile(profile string) bool {
	return profile == "small" || profile == "medium" || profile == "large"
}

func EffectiveResourceProfile(profile string) string {
	if profile == "" {
		return "small"
	}
	return profile
}

// ProfileResources owns only CPU request and memory request/limit. The capacity
// table is provisional; activation requires a capacity review.
func ProfileResources(profile string) corev1.ResourceRequirements {
	request, limit := "128Mi", "256Mi"
	switch profile {
	case "medium":
		request, limit = "256Mi", "512Mi"
	case "large":
		request, limit = "512Mi", "1Gi"
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse(request)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)},
	}
}

func ResourcesMatchProfile(resources corev1.ResourceRequirements, profile string) bool {
	want := ProfileResources(profile)
	return resources.Requests.Cpu().Equal(*want.Requests.Cpu()) &&
		resources.Requests.Memory().Equal(*want.Requests.Memory()) &&
		resources.Limits.Memory().Equal(*want.Limits.Memory())
}
