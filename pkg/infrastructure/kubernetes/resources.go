package kubernetes

import (
	"reflect"

	"github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
)

// MergeResourceOwnership is shared by API, migration, adoption and reconciliation
// writers. Replicas, sidecar resources, CPU limits and unowned resource keys stay live.
func MergeResourceOwnership(live, desired *unleashv1.Unleash, profile string, enabled bool) {
	desired.Spec.Size = live.Spec.Size
	for i := range desired.Spec.ExtraContainers {
		for _, container := range live.Spec.ExtraContainers {
			if desired.Spec.ExtraContainers[i].Name == container.Name {
				desired.Spec.ExtraContainers[i].Resources = *container.Resources.DeepCopy()
				break
			}
		}
	}
	desired.Spec.Resources = *live.Spec.Resources.DeepCopy()
	if profile == "" || !enabled {
		return
	}
	want := unleash.ProfileResources(profile)
	if desired.Spec.Resources.Requests == nil {
		desired.Spec.Resources.Requests = want.Requests
	} else {
		for key, value := range want.Requests {
			desired.Spec.Resources.Requests[key] = value
		}
	}
	if desired.Spec.Resources.Limits == nil {
		desired.Spec.Resources.Limits = want.Limits
	} else {
		for key, value := range want.Limits {
			desired.Spec.Resources.Limits[key] = value
		}
	}
}

func profileOnlyChange(previous, next *unleash.Config) bool {
	a, b := *previous, *next
	a.ResourceProfile, b.ResourceProfile = "", ""
	return reflect.DeepEqual(a, b)
}
