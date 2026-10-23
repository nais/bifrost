package unleash

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestProfileResourceTable(t *testing.T) {
	for _, tc := range []struct{ profile, request, limit string }{
		{"", "128Mi", "256Mi"},
		{"small", "128Mi", "256Mi"},
		{"medium", "256Mi", "512Mi"},
		{"large", "512Mi", "1Gi"},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			got := ProfileResources(tc.profile)
			assert.True(t, got.Requests.Cpu().Equal(resource.MustParse("100m")))
			assert.True(t, got.Requests.Memory().Equal(resource.MustParse(tc.request)))
			assert.True(t, got.Limits.Memory().Equal(resource.MustParse(tc.limit)))
			assert.Len(t, got.Requests, 2)
			assert.Len(t, got.Limits, 1, "new instances must not set a CPU limit")
		})
	}
}
