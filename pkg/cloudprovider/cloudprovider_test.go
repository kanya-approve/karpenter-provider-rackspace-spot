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
// pickInstanceType tests, and Get looks names up in that same list.
type stubInstanceTypeProvider struct {
	list []*karpcloudprovider.InstanceType
}

var _ instancetype.Provider = (*stubInstanceTypeProvider)(nil)

func (s *stubInstanceTypeProvider) List(context.Context, string) ([]*karpcloudprovider.InstanceType, error) {
	return s.list, nil
}

func (s *stubInstanceTypeProvider) Get(_ context.Context, _, name string) (*karpcloudprovider.InstanceType, error) {
	for _, it := range s.list {
		if it.Name == name {
			return it, nil
		}
	}
	return nil, errors.New("not found")
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
// assert which pool kind was targeted. A non-empty serverClass stands in for a
// pool that already exists with that class.
type fakeInstanceProvider struct {
	deleted     string
	assigned    bool
	serverClass string
}

var _ instance.Provider = (*fakeInstanceProvider)(nil)

func (f *fakeInstanceProvider) Create(_ context.Context, _ *apiv1.RackspaceSpotNodeClass, nc *karpv1.NodeClaim, its []*karpcloudprovider.InstanceType, _ string) (*instance.Pool, error) {
	name := string(nc.UID)
	serverClass := f.serverClass
	if serverClass == "" {
		serverClass = its[0].Name
	}
	return &instance.Pool{Name: name, ServerClass: serverClass, ProviderID: instance.MakeProviderID(testCloudspace, instance.PoolTypeSpot, name)}, nil
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
	nodeClass := &apiv1.RackspaceSpotNodeClass{}
	nodeClass.Name = "default"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeClass).Build()

	nc := &karpv1.NodeClaim{}
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
		})
	}
}

// A retry finds the pool from an earlier attempt, whose class may no longer be
// the cheapest. The NodeClaim must describe that pool's class, not the new
// pick, or it carries the wrong instance-type label and capacity.
func TestCreate_RetryKeepsPoolServerClass(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	nodeClass := &apiv1.RackspaceSpotNodeClass{}
	nodeClass.Name = "default"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeClass).Build()

	large := makeInstanceType("ch.large", 0.02, 0.06)
	large.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
	}
	c := &CloudProvider{
		kubeClient: kubeClient,
		// The pool was created as ch.large, but mh.medium is cheapest now.
		instances: &fakeInstanceProvider{assigned: true, serverClass: "ch.large"},
		instanceType: &stubInstanceTypeProvider{list: []*karpcloudprovider.InstanceType{
			makeInstanceType("mh.medium", 0.01, 0.03),
			large,
		}},
		cloudspace: testCloudspace,
		region:     testRegion,
	}

	nc := newClaim(karpv1.CapacityTypeSpot, "mh.medium", "ch.large")
	nc.UID = "uid-2"
	nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Kind: NodeClassKind, Name: "default"}

	got, err := c.Create(context.Background(), nc)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if it := got.Labels[corev1.LabelInstanceTypeStable]; it != "ch.large" {
		t.Errorf("instance-type label = %q, want ch.large (the pool's class)", it)
	}
	if cpu := got.Status.Capacity[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("cpu capacity = %s, want 4 (ch.large)", cpu.String())
	}
}
