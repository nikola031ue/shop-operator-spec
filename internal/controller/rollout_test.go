package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Spec 005 D-5 (research R10): a rollout is done only when no copy with earlier settings is left.
func TestRolloutInProgress(t *testing.T) {
	one := int32(1)
	cases := []struct {
		name   string
		status appsv1.DeploymentStatus
		want   bool
	}{
		{"all copies updated and available", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3}, false},
		{"generation not observed yet", appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3}, true},
		{"not every copy updated", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 2, AvailableReplicas: 3}, true},
		// The 004 T025 window: all desired copies updated, but an old one still runs and counts as available.
		{"an old copy still runs", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 4, UpdatedReplicas: 3, AvailableReplicas: 4}, true},
		{"an old copy is terminating", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, TerminatingReplicas: &one}, true},
	}
	for _, c := range cases {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: c.status}
		if got := RolloutInProgress(d, 3); got != c.want {
			t.Errorf("%s: RolloutInProgress = %v, want %v", c.name, got, c.want)
		}
	}
}
