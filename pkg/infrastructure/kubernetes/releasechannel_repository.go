package kubernetes

import (
	"context"
	"fmt"

	"github.com/nais/bifrost/pkg/domain/releasechannel"
	releasechannelv1 "github.com/nais/unleasherator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

// ReleaseChannelRepository implements the releasechannel.Repository interface for Kubernetes
type ReleaseChannelRepository struct {
	client    ctrl.Client
	namespace string
}

// NewReleaseChannelRepository creates a new Kubernetes-backed ReleaseChannel repository
func NewReleaseChannelRepository(client ctrl.Client, namespace string) *ReleaseChannelRepository {
	return &ReleaseChannelRepository{
		client:    client,
		namespace: namespace,
	}
}

// List retrieves all ReleaseChannel resources from Kubernetes
func (r *ReleaseChannelRepository) List(ctx context.Context) ([]*releasechannel.Channel, error) {
	channelList := &releasechannelv1.ReleaseChannelList{}

	opts := []ctrl.ListOption{
		ctrl.InNamespace(r.namespace),
	}

	if err := r.client.List(ctx, channelList, opts...); err != nil {
		return nil, fmt.Errorf("failed to list release channels: %w", err)
	}

	channels := make([]*releasechannel.Channel, 0, len(channelList.Items))
	for i := range channelList.Items {
		channels = append(channels, convertToChannel(&channelList.Items[i]))
	}

	return channels, nil
}

// Get retrieves a single ReleaseChannel by name from Kubernetes
func (r *ReleaseChannelRepository) Get(ctx context.Context, name string) (*releasechannel.Channel, error) {
	channel := &releasechannelv1.ReleaseChannel{}

	key := ctrl.ObjectKey{
		Name:      name,
		Namespace: r.namespace,
	}

	if err := r.client.Get(ctx, key, channel); err != nil {
		return nil, fmt.Errorf("failed to get release channel %s: %w", name, err)
	}

	return convertToChannel(channel), nil
}

// convertToChannel converts a Kubernetes ReleaseChannel CRD to our domain model
func convertToChannel(crd *releasechannelv1.ReleaseChannel) *releasechannel.Channel {
	var lastReconcileTime metav1.Time
	if crd.Status.LastReconcileTime != nil {
		lastReconcileTime = *crd.Status.LastReconcileTime
	}

	return &releasechannel.Channel{
		Name:      crd.Name,
		Image:     string(crd.Spec.Image),
		CreatedAt: crd.CreationTimestamp.Time,
		Spec: releasechannel.ChannelSpec{
			MaxParallel:   crd.Spec.Strategy.MaxParallel,
			CanaryEnabled: crd.Spec.Strategy.Canary.Enabled,
		},
		Status: releasechannel.ChannelStatus{
			Phase:             string(crd.Status.Phase),
			Version:           crd.Status.Version,
			Instances:         crd.Status.Instances,
			InstancesUpToDate: crd.Status.InstancesUpToDate,
			Progress:          crd.Status.Progress,
			Completed:         crd.Status.Rollout,
			FailureReason:     crd.Status.FailureReason,
			LastReconcileTime: lastReconcileTime,
			Conditions:        crd.Status.Conditions,
		},
	}
}
