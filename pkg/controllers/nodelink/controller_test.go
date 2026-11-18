/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package nodelink

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
)

const (
	poolUID        = "9f690e7a-7ec1-4dc6-82fd-1e0bef4dff77"
	provisionalID  = "rackspacespot://my-cs/spot/" + poolUID
	openstackID    = "openstack:///124fb8ef-cbaa-4d45-b300-9c9a9552cca7"
	machineForPool = poolUID + "-bvdch-gbkjh"
)

func newNodeClaim() *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{}
	nc.Name = "ci-kb2zj"
	nc.UID = poolUID
	nc.Status.ProviderID = provisionalID
	return nc
}

func reconcileNode(t *testing.T, node *corev1.Node, nc *karpv1.NodeClaim) *karpv1.NodeClaim {
	t.Helper()
	kubeClient := fake.NewClientBuilder().
		WithObjects(node, nc).
		WithStatusSubresource(&karpv1.NodeClaim{}).
		Build()
	if _, err := NewController(kubeClient).Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: node.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got karpv1.NodeClaim
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(nc), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestReconcile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		providerID  string
		want        string
	}{
		{
			name:       "linked by pool labels",
			labels:     map[string]string{instance.KarpenterManagedLabel: "true", instance.NodeClaimNameLabel: "ci-kb2zj"},
			providerID: openstackID,
			want:       openstackID,
		},
		{
			// Rackspace's Cluster API sets the machine annotation within
			// seconds of the Node joining but copies the pool labels minutes
			// later; waiting for them runs claims past the registration timeout.
			name:        "linked by machine annotation before labels sync",
			annotations: map[string]string{instance.MachineAnnotation: machineForPool},
			providerID:  openstackID,
			want:        openstackID,
		},
		{
			name:        "machine from a pool that is not a NodeClaim",
			annotations: map[string]string{instance.MachineAnnotation: "63351fa3-33fb-4048-a8de-9a4aa15fcb51-x8k2p-q7z4m"},
			providerID:  openstackID,
			want:        provisionalID,
		},
		{
			name:        "CCM has not set providerID yet",
			annotations: map[string]string{instance.MachineAnnotation: machineForPool},
			want:        provisionalID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{}
			node.Name = "prod-instance-1"
			node.Labels = tc.labels
			node.Annotations = tc.annotations
			node.Spec.ProviderID = tc.providerID

			got := reconcileNode(t, node, newNodeClaim())
			if got.Status.ProviderID != tc.want {
				t.Errorf("providerID = %q, want %q", got.Status.ProviderID, tc.want)
			}
		})
	}
}

// The UID must be followed by the machineset suffix's separator, so one
// UID being a prefix of another string can't link the wrong claim.
func TestNodeClaimFor_RequiresFullPoolName(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "prod-instance-1"
	node.Annotations = map[string]string{instance.MachineAnnotation: poolUID + "0-bvdch-gbkjh"}
	node.Spec.ProviderID = openstackID

	if got := reconcileNode(t, node, newNodeClaim()); got.Status.ProviderID != provisionalID {
		t.Errorf("providerID = %q, want it left at %q", got.Status.ProviderID, provisionalID)
	}
}

func TestManagedNode(t *testing.T) {
	p := managedNode()
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		want        bool
	}{
		{"managed label", map[string]string{instance.KarpenterManagedLabel: "true"}, nil, true},
		{"machine annotation", nil, map[string]string{instance.MachineAnnotation: machineForPool}, true},
		{"neither", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{}
			node.Labels = tc.labels
			node.Annotations = tc.annotations
			if got := p.Create(event.CreateEvent{Object: node}); got != tc.want {
				t.Errorf("predicate = %v, want %v", got, tc.want)
			}
		})
	}
}
