/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cloudprovider

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
)

const (
	testRegion     = "us-central-dfw-1"
	testCloudspace = "my-cs"
)

// stubInstanceTypeProvider returns a fixed instance-type list for
// pickInstanceType tests; Get/MinBidPrice are unused on that path.
type stubInstanceTypeProvider struct {
	list []*karpcloudprovider.InstanceType
}

var _ instancetype.Provider = (*stubInstanceTypeProvider)(nil)

func (s *stubInstanceTypeProvider) List(context.Context, string) ([]*karpcloudprovider.InstanceType, error) {
	return s.list, nil
}

func (s *stubInstanceTypeProvider) Get(context.Context, string, string) (*karpcloudprovider.InstanceType, error) {
	return nil, errors.New("unused")
}

func (s *stubInstanceTypeProvider) MinBidPrice(context.Context, string, string) (float64, error) {
	return 0, nil
}

func (s *stubInstanceTypeProvider) UpdateFromNode(string, corev1.ResourceList) {}

// makeInstanceType mirrors instancetype.translate: one InstanceType with a spot
// and/or on-demand offering (priced only when > 0), all available.
func makeInstanceType(name string, spotPrice, onDemandPrice float64) *karpcloudprovider.InstanceType {
	it := &karpcloudprovider.InstanceType{
		Name: name,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, testRegion),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, karpv1.ArchitectureAmd64),
			scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		),
	}
	if onDemandPrice > 0 {
		it.Offerings = append(it.Offerings, &karpcloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, testRegion),
			),
			Price:     onDemandPrice,
			Available: true,
		})
	}
	if spotPrice > 0 {
		it.Offerings = append(it.Offerings, &karpcloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, testRegion),
			),
			Price:     spotPrice,
			Available: true,
		})
	}
	// translate always sets Capacity and Overhead; Allocatable() dereferences
	// Overhead, and cluster state reads Capacity off in-flight NodeClaims.
	it.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("2"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
		corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
	}
	it.Overhead = &karpcloudprovider.InstanceTypeOverhead{}
	return it
}

func newClaim(capacityType string, instanceTypeNames ...string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{}
	if capacityType != "" {
		nc.Spec.Requirements = append(nc.Spec.Requirements, karpv1.NodeSelectorRequirementWithMinValues{
			Key:      karpv1.CapacityTypeLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{capacityType},
		})
	}
	if len(instanceTypeNames) > 0 {
		nc.Spec.Requirements = append(nc.Spec.Requirements, karpv1.NodeSelectorRequirementWithMinValues{
			Key:      corev1.LabelInstanceTypeStable,
			Operator: corev1.NodeSelectorOpIn,
			Values:   instanceTypeNames,
		})
	}
	return nc
}

func TestPickInstanceType_ChoosesCheapestSpot(t *testing.T) {
	c := &CloudProvider{
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("gp.large", 0.05, 0.10),
			makeInstanceType("gp.small", 0.01, 0.03), // cheapest spot
			makeInstanceType("gp.xl", 0.08, 0.20),
		}},
		region: testRegion,
	}
	it, ct, err := c.pickInstanceType(context.Background(), newClaim(karpv1.CapacityTypeSpot, "gp.large", "gp.small", "gp.xl"), testRegion)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if it.Name != "gp.small" {
		t.Errorf("picked %q, want gp.small (cheapest spot)", it.Name)
	}
	if ct != karpv1.CapacityTypeSpot {
		t.Errorf("capacity type = %q, want spot", ct)
	}
}

// The cheapest type overall is excluded from the NodeClaim's allow-list, so it
// must not be picked — guards the instance-type Intersects filter.
func TestPickInstanceType_RespectsAllowedTypes(t *testing.T) {
	c := &CloudProvider{
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("gp.cheap", 0.001, 0.002), // cheapest overall, NOT allowed
			makeInstanceType("gp.mid", 0.02, 0.04),
			makeInstanceType("gp.big", 0.05, 0.10),
		}},
		region: testRegion,
	}
	it, _, err := c.pickInstanceType(context.Background(), newClaim(karpv1.CapacityTypeSpot, "gp.mid", "gp.big"), testRegion)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if it.Name != "gp.mid" {
		t.Errorf("picked %q, want gp.mid (cheapest among allowed; gp.cheap excluded)", it.Name)
	}
}

// On-demand must compare on-demand prices, not spot — guards the capacity-type
// scoping of the price comparison.
func TestPickInstanceType_OnDemandUsesOnDemandPrice(t *testing.T) {
	c := &CloudProvider{
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("a", 0.01, 0.30), // cheapest spot, dearest on-demand
			makeInstanceType("b", 0.20, 0.05), // cheapest on-demand
		}},
		region: testRegion,
	}
	it, ct, err := c.pickInstanceType(context.Background(), newClaim(karpv1.CapacityTypeOnDemand, "a", "b"), testRegion)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if it.Name != "b" {
		t.Errorf("picked %q, want b (cheapest on-demand)", it.Name)
	}
	if ct != karpv1.CapacityTypeOnDemand {
		t.Errorf("capacity type = %q, want on-demand", ct)
	}
}

func TestPickInstanceType_NoneCompatible(t *testing.T) {
	c := &CloudProvider{
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("gp.a", 0.01, 0.02),
		}},
		region: testRegion,
	}
	if _, _, err := c.pickInstanceType(context.Background(), newClaim(karpv1.CapacityTypeSpot, "nonexistent"), testRegion); err == nil {
		t.Error("expected error when no instance type matches, got nil")
	}
}

// When a NodeClaim allows both capacity types, spot is preferred even if an
// on-demand offering is cheaper overall.
func TestPickInstanceType_PrefersSpotWhenAllowed(t *testing.T) {
	c := &CloudProvider{
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("gp.small", 0.03, 0.01), // cheapest overall is its on-demand
			makeInstanceType("gp.large", 0.02, 0.04), // cheapest spot
		}},
		region: testRegion,
	}
	nc := newClaim("", "gp.small", "gp.large")
	nc.Spec.Requirements = append(nc.Spec.Requirements, karpv1.NodeSelectorRequirementWithMinValues{
		Key:      karpv1.CapacityTypeLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{karpv1.CapacityTypeOnDemand, karpv1.CapacityTypeSpot},
	})
	it, ct, err := c.pickInstanceType(context.Background(), nc, testRegion)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ct != karpv1.CapacityTypeSpot {
		t.Errorf("capacity type = %q, want spot (preferred when allowed)", ct)
	}
	if it.Name != "gp.large" {
		t.Errorf("picked %q, want gp.large (cheapest spot, not the cheaper on-demand)", it.Name)
	}
}

// fakeInstanceProvider records the providerID passed to Delete so the test can
// assert which pool kind was targeted.
type fakeInstanceProvider struct {
	deleted  string
	assigned bool
}

var _ instance.Provider = (*fakeInstanceProvider)(nil)

func (f *fakeInstanceProvider) Create(_ context.Context, _ *apiv1.RackspaceSpotNodeClass, nc *karpv1.NodeClaim, _ []*karpcloudprovider.InstanceType, _ string) (*instance.Pool, error) {
	name := string(nc.UID)
	return &instance.Pool{Name: name, ProviderID: instance.MakeProviderID(testCloudspace, instance.PoolTypeSpot, name)}, nil
}
func (f *fakeInstanceProvider) Get(context.Context, string) (*instance.Pool, error) {
	return nil, instance.ErrPoolNotFound
}
func (f *fakeInstanceProvider) Delete(_ context.Context, providerID string) error {
	f.deleted = providerID
	return nil
}
func (f *fakeInstanceProvider) List(context.Context) ([]*instance.Pool, error) { return nil, nil }
func (f *fakeInstanceProvider) ServerAssigned(context.Context, string) (bool, error) {
	return f.assigned, nil
}
func (f *fakeInstanceProvider) Cloudspace() string { return testCloudspace }

// Delete must target the pool kind Create actually built — read from the
// capacity-type label — not the requirement's first value, which can disagree
// when both capacity types are allowed. Deleting the wrong kind leaks the pool.
func TestDelete_UsesCapacityTypeLabel(t *testing.T) {
	f := &fakeInstanceProvider{}
	c := &CloudProvider{instances: f, cloudspace: testCloudspace}

	nc := &karpv1.NodeClaim{}
	nc.UID = "uid-9"
	nc.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{
		Key:      karpv1.CapacityTypeLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{karpv1.CapacityTypeOnDemand, karpv1.CapacityTypeSpot}, // on-demand first
	}}
	nc.Labels = map[string]string{karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot} // Create chose spot

	if err := c.Delete(context.Background(), nc); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := instance.MakeProviderID(testCloudspace, instance.PoolTypeSpot, "uid-9")
	if f.deleted != want {
		t.Errorf("deleted %q, want %q (kind from capacity-type label, not requirement)", f.deleted, want)
	}
}

// The error type is the contract: a plain error also happens to hold
// Launched=Unknown today, but only CreateError sets the condition reason.
func TestCreate_AwaitsServerAssignment(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	nodeClass := &apiv1.RackspaceSpotNodeClass{}
	nodeClass.Name = "default"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeClass).Build()

	nc := &karpv1.NodeClaim{}
	nc.Name = "default-abc12"
	nc.UID = "uid-1"
	nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Kind: NodeClassKind, Name: "default"}
	nc.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{Key: karpv1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeSpot}},
		{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"gp.small"}},
	}

	for _, tc := range []struct {
		name       string
		assigned   bool
		wantCreate bool
	}{
		{"no server attached yet", false, false},
		{"server attached", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInstanceProvider{assigned: tc.assigned}
			c := &CloudProvider{
				kubeClient: kubeClient,
				instances:  f,
				instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
					makeInstanceType("gp.small", 0.01, 0.03),
				}},
				cloudspace: testCloudspace,
				region:     testRegion,
			}

			claim := nc.DeepCopy()
			_, err := c.Create(context.Background(), claim)
			if tc.wantCreate {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Create succeeded, want a retryable CreateError; NodeClaim would be marked Launched before the VM exists")
			}
			var createErr *karpcloudprovider.CreateError
			if !errors.As(err, &createErr) {
				t.Fatalf("Create error = %T (%v), want *cloudprovider.CreateError so karpenter keeps Launched=Unknown", err, err)
			}
			if createErr.ConditionReason != "AwaitingServerAssignment" {
				t.Errorf("ConditionReason = %q, want AwaitingServerAssignment", createErr.ConditionReason)
			}
			// Cluster.Synced() blocks cluster-wide on an empty providerID, and
			// StateNode.Capacity() reads zero without capacity, which makes the
			// scheduler launch a second claim for the same pod.
			if claim.Status.ProviderID == "" {
				t.Error("ProviderID is empty while awaiting assignment; Cluster.Synced() will block provisioning cluster-wide")
			}
			if len(claim.Status.Capacity) == 0 {
				t.Error("Capacity is empty while awaiting assignment; the scheduler will provision a duplicate claim")
			}
			if claim.Labels[corev1.LabelInstanceTypeStable] != "gp.small" {
				t.Errorf("instance-type label = %q, want gp.small", claim.Labels[corev1.LabelInstanceTypeStable])
			}
			// Karpenter copies NodeClaim labels onto the Node at registration;
			// Get and List find the Node by these, long before Rackspace syncs
			// the pool's copies.
			for k, want := range map[string]string{
				instance.KarpenterManagedLabel: "true",
				instance.NodeClaimNameLabel:    "default-abc12",
				instance.NodeClaimUIDLabel:     "uid-1",
			} {
				if got := claim.Labels[k]; got != want {
					t.Errorf("label %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// AssignedServers can lag minutes behind the VM joining, and that wait comes
// out of the registration timeout. A Node from the pool counts as assigned, and
// its providerID is returned so nodelink has nothing left to rewrite.
func TestCreate_JoinedNodeCountsAsAssigned(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	nodeClass := &apiv1.RackspaceSpotNodeClass{}
	nodeClass.Name = "default"

	for _, tc := range []struct {
		name           string
		machine        string
		providerID     string
		wantProviderID string // empty = Create should still be waiting
	}{
		{"node from this pool", "uid-1-bvdch-gbkjh", "openstack:///vm-1", "openstack:///vm-1"},
		{"node from another pool", "uid-10-bvdch-gbkjh", "openstack:///vm-1", ""},
		{"node without a providerID yet", "uid-1-bvdch-gbkjh", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{}
			node.Name = "prod-instance-1"
			node.Annotations = map[string]string{instance.MachineAnnotation: tc.machine}
			node.Spec.ProviderID = tc.providerID
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeClass, node).Build()

			c := &CloudProvider{
				kubeClient: kubeClient,
				instances:  &fakeInstanceProvider{assigned: false},
				instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
					makeInstanceType("gp.small", 0.01, 0.03),
				}},
				cloudspace: testCloudspace,
				region:     testRegion,
			}

			nc := &karpv1.NodeClaim{}
			nc.Name = "default-abc12"
			nc.UID = "uid-1"
			nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Kind: NodeClassKind, Name: "default"}
			nc.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: karpv1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeSpot}},
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"gp.small"}},
			}

			got, err := c.Create(context.Background(), nc)
			if tc.wantProviderID == "" {
				var createErr *karpcloudprovider.CreateError
				if !errors.As(err, &createErr) {
					t.Fatalf("Create = %v, want an AwaitingServerAssignment CreateError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got.Status.ProviderID != tc.wantProviderID {
				t.Errorf("providerID = %q, want %q", got.Status.ProviderID, tc.wantProviderID)
			}
		})
	}
}
