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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
)

const testRegion = "us-central-dfw-1"

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
