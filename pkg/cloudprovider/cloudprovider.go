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

	"github.com/awslabs/operatorpkg/status"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/operator"
)

// Name is the CloudProvider identifier, also used as the karpenter.sh/managed-by value.
const Name = "rackspacespot"

var errNotImplemented = errors.New("not implemented (MVP scaffold)")

// CloudProvider implements sigs.k8s.io/karpenter/pkg/cloudprovider.CloudProvider
// for Rackspace Spot.
//
// Method bodies are intentionally stubbed at this stage; concrete behavior is
// filled in by subsequent tasks (instance/instancetype/pricing providers).
type CloudProvider struct {
	op *operator.Operator
}

var _ karpcloudprovider.CloudProvider = (*CloudProvider)(nil)

func New(op *operator.Operator) *CloudProvider {
	return &CloudProvider{op: op}
}

func (c *CloudProvider) Create(ctx context.Context, claim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	return nil, errNotImplemented
}

func (c *CloudProvider) Delete(ctx context.Context, claim *karpv1.NodeClaim) error {
	// Returning NodeClaimNotFound makes Karpenter consider the claim already gone,
	// which is the correct stub behavior — we never created anything.
	return karpcloudprovider.NewNodeClaimNotFoundError(errNotImplemented)
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	return nil, karpcloudprovider.NewNodeClaimNotFoundError(errNotImplemented)
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	return nil, nil
}

func (c *CloudProvider) GetInstanceTypes(ctx context.Context, pool *karpv1.NodePool) ([]*karpcloudprovider.InstanceType, error) {
	return nil, nil
}

func (c *CloudProvider) IsDrifted(ctx context.Context, claim *karpv1.NodeClaim) (karpcloudprovider.DriftReason, error) {
	// Drift detection is post-MVP — see kanya-approve/karpenter-provider-rackspace-spot#1.
	return "", nil
}

func (c *CloudProvider) RepairPolicies() []karpcloudprovider.RepairPolicy {
	// Repair policies are post-MVP — see kanya-approve/karpenter-provider-rackspace-spot#3.
	return nil
}

func (c *CloudProvider) Name() string {
	return Name
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&apiv1.RackspaceSpotNodeClass{}}
}
