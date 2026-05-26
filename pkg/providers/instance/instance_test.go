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
	"strings"
	"testing"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	rxtmocks "github.com/rackspace-spot/spot-go-sdk/api/v1/mocks"
	gomock "go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	testOrgID      = "rxt-org-1"
	testCloudspace = "my-cs"
	testServerCls  = "gp.vs1.small-dfw"
)

// composedAPI satisfies instance.API by combining the three per-resource SDK
// mocks. We use it instead of MockSpotAPI because the SDK's super-mock has a
// stale signature for SpotPricingAPI.GetMarketPriceForServerClass (it returns
// (string, error) while the real interface returns string).
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

func newClaim(uid string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "claim-" + uid,
			UID:  types.UID(uid),
		},
	}
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
	nc := newClaim("abc-123")
	if got, want := PoolName(nc), PoolNamePrefix+"abc-123"; got != want {
		t.Errorf("PoolName = %q, want %q", got, want)
	}
}

func TestCreateSpot_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api)

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

	nc := newClaim("uid-1")
	pool, err := p.Create(context.Background(), nc, CreateOptions{
		Cloudspace:   testCloudspace,
		ServerClass:  testServerCls,
		BidPrice:     "0.05",
		CapacityType: karpv1.CapacityTypeSpot,
		Labels:       map[string]string{"team": "platform"},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if pool.Type != PoolTypeSpot {
		t.Errorf("pool.Type = %q, want %q", pool.Type, PoolTypeSpot)
	}
	if pool.ProviderID != MakeProviderID(testCloudspace, PoolTypeSpot, PoolName(nc)) {
		t.Errorf("unexpected providerID %q", pool.ProviderID)
	}
	if captured.BidPrice != "0.05" {
		t.Errorf("BidPrice = %q, want 0.05", captured.BidPrice)
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

func TestCreateSpot_RequiresBidPrice(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api)

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	// No CreateSpotNodePool call expected.

	_, err := p.Create(context.Background(), newClaim("uid-2"), CreateOptions{
		Cloudspace:   testCloudspace,
		ServerClass:  testServerCls,
		CapacityType: karpv1.CapacityTypeSpot,
	})
	if err == nil || !strings.Contains(err.Error(), "bid price required") {
		t.Fatalf("expected bid-price-required error, got %v", err)
	}
}

func TestCreateOnDemand_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api)

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		CreateOnDemandNodePool(gomock.Any(), testOrgID, gomock.Any()).Return(nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		GetOnDemandNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(&rxtspot.OnDemandNodePool{
			Name:        PoolName(newClaim("uid-3")),
			Org:         testOrgID,
			Cloudspace:  testCloudspace,
			ServerClass: testServerCls,
			Desired:     1,
		}, nil)

	pool, err := p.Create(context.Background(), newClaim("uid-3"), CreateOptions{
		Cloudspace:   testCloudspace,
		ServerClass:  testServerCls,
		CapacityType: karpv1.CapacityTypeOnDemand,
	})
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
	p := NewProvider(api)

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockSpotNodePoolAPI.EXPECT().
		CreateSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(errors.New("HTTP 409: AlreadyExists"))
	api.MockSpotNodePoolAPI.EXPECT().
		GetSpotNodePool(gomock.Any(), testOrgID, gomock.Any()).
		Return(&rxtspot.SpotNodePool{Name: PoolName(newClaim("uid-4")), BidPrice: "0.05"}, nil)

	_, err := p.Create(context.Background(), newClaim("uid-4"), CreateOptions{
		Cloudspace:   testCloudspace,
		ServerClass:  testServerCls,
		BidPrice:     "0.05",
		CapacityType: karpv1.CapacityTypeSpot,
	})
	if err != nil {
		t.Fatalf("expected idempotent success on AlreadyExists, got %v", err)
	}
}

func TestDelete_NotFoundMapsToErrPoolNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	api := newAPI(ctrl)
	p := NewProvider(api)

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
	p := NewProvider(api)

	api.MockOrganizationAPI.EXPECT().ListOrganizations(gomock.Any()).
		Return([]rxtspot.Organization{{ID: testOrgID}}, nil)
	api.MockSpotNodePoolAPI.EXPECT().
		ListSpotNodePools(gomock.Any(), testOrgID, testCloudspace).
		Return([]*rxtspot.SpotNodePool{
			{Name: "karpenter-uid-a", Cloudspace: testCloudspace, ServerClass: testServerCls},
			{Name: "manually-managed", Cloudspace: testCloudspace, ServerClass: testServerCls},
		}, nil)
	api.MockOnDemandNodePoolAPI.EXPECT().
		ListOnDemandNodePools(gomock.Any(), testOrgID, testCloudspace).
		Return([]*rxtspot.OnDemandNodePool{
			{Name: "karpenter-uid-b", Cloudspace: testCloudspace, ServerClass: testServerCls},
		}, nil)

	pools, err := p.List(context.Background(), testCloudspace)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("expected 2 karpenter-managed pools, got %d", len(pools))
	}
	for _, pool := range pools {
		if !strings.HasPrefix(pool.Name, PoolNamePrefix) {
			t.Errorf("List returned a foreign pool %q", pool.Name)
		}
	}
}
