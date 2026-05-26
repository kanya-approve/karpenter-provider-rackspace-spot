/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package instance

import (
	"context"
	"errors"
	"testing"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	rxtmocks "github.com/rackspace-spot/spot-go-sdk/api/v1/mocks"
	gomock "go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/pricing"
)

// stubPricing always errors on Percentiles, which exercises the
// market*1.2 fallback path in chooseBidPrice.
type stubPricing struct{}

func (*stubPricing) SpotPrice(*rxtspot.ServerClass) float64                                    { return 0 }
func (*stubPricing) OnDemandPrice(*rxtspot.ServerClass) float64                                { return 0 }
func (*stubPricing) MinBidPrice(*rxtspot.ServerClass) float64                                  { return 0 }
func (*stubPricing) Percentiles(context.Context, string, string) (pricing.Percentiles, error) {
	return pricing.Percentiles{}, errors.New("stub: no feed")
}

// stubInstanceType returns ErrNotFound for MinBidPrice so chooseBidPrice
// falls through without clamping. List/Get aren't called in instance tests.
type stubInstanceType struct{}

func (*stubInstanceType) List(context.Context, string) ([]*karpcloudprovider.InstanceType, error) {
	return nil, nil
}

func (*stubInstanceType) Get(context.Context, string, string) (*karpcloudprovider.InstanceType, error) {
	return nil, errors.New("stub")
}

func (*stubInstanceType) MinBidPrice(context.Context, string, string) (float64, error) {
	return 0, errors.New("stub: no min bid")
}

const (
	testOrgID      = "rxt-org-1"
	testCloudspace = "my-cs"
	testServerCls  = "gp.vs1.small-dfw"
)

// composedAPI satisfies instance.API by combining the three per-resource SDK
// mocks. We use it instead of MockSpotAPI because the SDK's super-mock has a
// stale signature for SpotPricingAPI.GetMarketPriceForServerClass.
type composedAPI struct {
	*rxtmocks.MockSpotNodePoolAPI
	*rxtmocks.MockOnDemandNodePoolAPI
	*rxtmocks.MockOrganizationAPI
}

func newAPI(ctrl *gomock.Controller) *composedAPI {
	return &composedAPI{
		MockSpotNodePoolAPI:     rxtmocks.NewMockSpotNodePoolAPI(ctrl),
		MockOnDemandNodePoolAPI: rxtmocks.NewMockOnDemandNodePoolAPI(ctrl),
		MockOrganizationAPI:     rxtmocks.NewMockOrganizationAPI(ctrl),
	}
}

func newClaim(uid, capacityType string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-" + uid, UID: types.UID(uid)},
	}
	if capacityType != "" {
		nc.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{
			Key:      karpv1.CapacityTypeLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{capacityType},
		}}
	}
	return nc
}

func newNodeClass(extraLabels map[string]string) *apiv1.RackspaceSpotNodeClass {
	return &apiv1.RackspaceSpotNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: apiv1.RackspaceSpotNodeClassSpec{
			CloudspaceName: testCloudspace,
			Labels:         extraLabels,
		},
	}
}

func newInstanceTypes() []*karpcloudprovider.InstanceType {
	spot := &karpcloudprovider.Offering{
		Requirements: scheduling.NewRequirements(scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot)),
		Price:        0.001,
		Available:    true,
	}
	return []*karpcloudprovider.InstanceType{{Name: testServerCls, Offerings: karpcloudprovider.Offerings{spot}}}
}

func TestProviderIDRoundTrip(t *testing.T) {
	cases := []struct {
		name                   string
		cloudspace, kind, pool string
	}{
		{"spot", testCloudspace, PoolTypeSpot, "karpenter-abc"},
		{"on-demand", testCloudspace, PoolTypeOnDemand, "karpenter-xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := MakeProviderID(tc.cloudspace, tc.kind, tc.pool)
			cs, k, n, err := ParseProviderID(id)
			if err != nil {
				t.Fatalf("ParseProviderID(%q) error = %v", id, err)
			}
			if cs != tc.cloudspace || k != tc.kind || n != tc.pool {
				t.Errorf("round-trip mismatch: got (%q,%q,%q), want (%q,%q,%q)", cs, k, n, tc.cloudspace, tc.kind, tc.pool)
			}
		})
	}
}

func TestParseProviderID_Rejects(t *testing.T) {
	cases := []string{
		"",
		"http://example.com/a/b/c",
		"rackspacespot://only-two/parts",
	}
	for _, in := range cases {
		if _, _, _, err := ParseProviderID(in); err == nil {
			t.Errorf("ParseProviderID(%q) expected error, got nil", in)
		}
	}
}

func TestPoolNameIsDeterministic(t *testing.T) {
	nc := newClaim("abc-123", "")
	if got, want := PoolName(nc), "abc-123"; got != want {
		t.Errorf("PoolName = %q, want %q", got, want)
	}
}

func TestDeriveCapacityType_DefaultsToOnDemand(t *testing.T) {
	if got := deriveCapacityType(newClaim("x", "")); got != karpv1.CapacityTypeOnDemand {
		t.Errorf("default capacity type = %q, want on-demand", got)
	}
}

func TestCreateSpot_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api, &stubPricing{}, &stubInstanceType{})

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID, Name: "test"}}, nil)

	var captured rxtspot.SpotNodePool
	api.MockSpotNodePoolAPI.EXPECT().
		CreateSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, pool rxtspot.SpotNodePool) error {
			captured = pool
			return nil
		})
	api.MockSpotNodePoolAPI.EXPECT().
		GetSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, name string) (*rxtspot.SpotNodePool, error) {
			c := captured
			c.Name = name
			c.Status = "Pending"
			return &c, nil
		})

	nc := newClaim("uid-1", karpv1.CapacityTypeSpot)
	nodeClass := newNodeClass(map[string]string{"team": "platform"})

	pool, err := p.Create(context.Background(), nodeClass, nc, newInstanceTypes())
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if pool.Type != PoolTypeSpot {
		t.Errorf("pool.Type = %q, want %q", pool.Type, PoolTypeSpot)
	}
	if pool.ProviderID != MakeProviderID(testCloudspace, PoolTypeSpot, PoolName(nc)) {
		t.Errorf("unexpected providerID %q", pool.ProviderID)
	}
	// Market 0.001 * 1.2 = 0.0012, ceil to 3 decimals = 0.002.
	// Rackspace's CRD validation only accepts up to 3 dp.
	if captured.BidPrice != "0.002" {
		t.Errorf("BidPrice = %q, want 0.002", captured.BidPrice)
	}
	if captured.Desired != 1 {
		t.Errorf("Desired = %d, want 1", captured.Desired)
	}
	if captured.CustomLabels[KarpenterManagedLabel] != "true" {
		t.Errorf("expected %s=true on pool labels", KarpenterManagedLabel)
	}
	if captured.CustomLabels[NodeClaimUIDLabel] != "uid-1" {
		t.Errorf("expected %s=uid-1 on pool labels", NodeClaimUIDLabel)
	}
	if captured.CustomLabels["team"] != "platform" {
		t.Errorf("custom label 'team' not propagated")
	}
}

func TestChooseBidPrice_MarketPlusHeadroomFallback(t *testing.T) {
	p := NewProvider(newAPI(gomock.NewController(t)), &stubPricing{}, &stubInstanceType{})
	got, err := p.chooseBidPrice(context.Background(), newNodeClass(nil), newInstanceTypes()[0])
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "0.002" {
		t.Errorf("chooseBidPrice (fallback) = %q, want 0.002 (market 0.001 * 1.2 ceil 3dp)", got)
	}
}

func TestChooseBidPrice_NoSpotOffering(t *testing.T) {
	p := NewProvider(newAPI(gomock.NewController(t)), &stubPricing{}, &stubInstanceType{})
	it := &karpcloudprovider.InstanceType{Name: testServerCls} // no offerings
	if _, err := p.chooseBidPrice(context.Background(), newNodeClass(nil), it); err == nil {
		t.Error("expected error when no spot offering, got nil")
	}
}

func TestCreateOnDemand_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api, &stubPricing{}, &stubInstanceType{})

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		CreateOnDemandNodePool(gomock.Any(), testOrgID, gomock.Any()).Return(nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		GetOnDemandNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(&rxtspot.OnDemandNodePool{
			Name:        PoolName(newClaim("uid-3", "")),
			Org:         testOrgID,
			Cloudspace:  testCloudspace,
			ServerClass: testServerCls,
			Desired:     1,
		}, nil)

	pool, err := p.Create(context.Background(),
		newNodeClass(nil),
		newClaim("uid-3", karpv1.CapacityTypeOnDemand),
		newInstanceTypes(),
	)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if pool.CapacityType != karpv1.CapacityTypeOnDemand {
		t.Errorf("CapacityType = %q, want on-demand", pool.CapacityType)
	}
}

func TestCreate_IdempotentOnAlreadyExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api, &stubPricing{}, &stubInstanceType{})

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockSpotNodePoolAPI.EXPECT().
		CreateSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(errors.New("HTTP 409: AlreadyExists"))
	api.MockSpotNodePoolAPI.EXPECT().
		GetSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(&rxtspot.SpotNodePool{Name: PoolName(newClaim("uid-4", "")), BidPrice: "0.05"}, nil)

	_, err := p.Create(context.Background(),
		newNodeClass(nil),
		newClaim("uid-4", karpv1.CapacityTypeSpot),
		newInstanceTypes(),
	)
	if err != nil {
		t.Fatalf("expected idempotent success on AlreadyExists, got %v", err)
	}
}

func TestDelete_NotFoundMapsToErrPoolNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api, &stubPricing{}, &stubInstanceType{})

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockSpotNodePoolAPI.EXPECT().
		DeleteSpotNodePool(gomock.Any(), testOrgID, "karpenter-uid-5").
		Return(errors.New("HTTP 404: NotFound"))

	err := p.Delete(context.Background(), MakeProviderID(testCloudspace, PoolTypeSpot, "karpenter-uid-5"))
	if !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("expected ErrPoolNotFound, got %v", err)
	}
}

func TestList_FiltersForeignPools(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api, &stubPricing{}, &stubInstanceType{})

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	karpLabel := map[string]string{KarpenterManagedLabel: "true"}
	api.MockSpotNodePoolAPI.EXPECT().
		ListSpotNodePools(gomock.Any(), testOrgID, testCloudspace).
		Return([]*rxtspot.SpotNodePool{
			{Name: "a-karpenter-pool", Cloudspace: testCloudspace, ServerClass: testServerCls, CustomLabels: karpLabel},
			{Name: "manually-managed", Cloudspace: testCloudspace, ServerClass: testServerCls},
		}, nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		ListOnDemandNodePools(gomock.Any(), testOrgID, testCloudspace).
		Return([]*rxtspot.OnDemandNodePool{
			{Name: "b-karpenter-pool", Cloudspace: testCloudspace, ServerClass: testServerCls, CustomLabels: karpLabel},
		}, nil)

	pools, err := p.List(context.Background(), testCloudspace)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("expected 2 karpenter-managed pools, got %d", len(pools))
	}
	for _, pool := range pools {
		if pool.Labels[KarpenterManagedLabel] != "true" {
			t.Errorf("List returned a foreign pool %q", pool.Name)
		}
	}
}
