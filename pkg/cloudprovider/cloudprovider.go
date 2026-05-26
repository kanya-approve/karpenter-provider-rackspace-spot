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
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/operator"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
)

// Name is the CloudProvider identifier, also used as the karpenter.sh/managed-by value.
const Name = "rackspacespot"

// NodeClassKind is the Kind value on NodePool.spec.template.spec.nodeClassRef
// that this provider responds to.
const NodeClassKind = "RackspaceSpotNodeClass"

// CloudProvider implements sigs.k8s.io/karpenter/pkg/cloudprovider.CloudProvider
// for Rackspace Spot.
type CloudProvider struct {
	kubeClient   client.Client
	instances    instance.Provider
	instanceType instancetype.Provider
	spotAPI      rxtspot.SpotAPI
}

var _ karpcloudprovider.CloudProvider = (*CloudProvider)(nil)

func New(op *operator.Operator) *CloudProvider {
	return &CloudProvider{
		kubeClient:   op.GetClient(),
		instances:    op.InstanceProvider,
		instanceType: op.InstanceTypeProvider,
		spotAPI:      op.SpotClient,
	}
}

func (c *CloudProvider) Name() string { return Name }

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&apiv1.RackspaceSpotNodeClass{}}
}

// IsDrifted: post-MVP — see issue #1.
func (c *CloudProvider) IsDrifted(ctx context.Context, nc *karpv1.NodeClaim) (karpcloudprovider.DriftReason, error) {
	return "", nil
}

// RepairPolicies: post-MVP — see issue #3.
func (c *CloudProvider) RepairPolicies() []karpcloudprovider.RepairPolicy {
	return nil
}

func (c *CloudProvider) Create(ctx context.Context, nc *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.resolveNodeClass(ctx, nc)
	if err != nil {
		return nil, fmt.Errorf("resolving node class: %w", err)
	}

	region, err := c.cloudspaceRegion(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving cloudspace region: %w", err)
	}

	instType, capacityType, err := c.pickInstanceType(ctx, nc, region)
	if err != nil {
		return nil, err
	}

	pool, err := c.instances.Create(ctx, nc, instance.CreateOptions{
		Cloudspace:   nodeClass.Spec.CloudspaceName,
		ServerClass:  instType.Name,
		BidPrice:     nodeClass.Spec.BidPrice,
		CapacityType: capacityType,
		Labels:       nodeClass.Spec.Labels,
		Annotations:  nodeClass.Spec.Annotations,
		Taints:       nodeClass.Spec.Taints,
	})
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	return c.hydrateClaim(nc, pool, instType, capacityType, region), nil
}

func (c *CloudProvider) Delete(ctx context.Context, nc *karpv1.NodeClaim) error {
	if nc.Status.ProviderID == "" {
		return karpcloudprovider.NewNodeClaimNotFoundError(errors.New("nodeclaim has no provider ID"))
	}
	if err := c.instances.Delete(ctx, nc.Status.ProviderID); err != nil {
		if errors.Is(err, instance.ErrPoolNotFound) {
			return karpcloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}
	return nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	pool, err := c.instances.Get(ctx, providerID)
	if err != nil {
		if errors.Is(err, instance.ErrPoolNotFound) {
			return nil, karpcloudprovider.NewNodeClaimNotFoundError(err)
		}
		return nil, err
	}
	return c.poolToClaim(ctx, pool), nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	var classes apiv1.RackspaceSpotNodeClassList
	if err := c.kubeClient.List(ctx, &classes); err != nil {
		return nil, fmt.Errorf("listing RackspaceSpotNodeClasses: %w", err)
	}
	cloudspaces := lo.Uniq(lo.Map(classes.Items, func(nc apiv1.RackspaceSpotNodeClass, _ int) string {
		return nc.Spec.CloudspaceName
	}))

	var claims []*karpv1.NodeClaim
	for _, cs := range cloudspaces {
		if cs == "" {
			continue
		}
		pools, err := c.instances.List(ctx, cs)
		if err != nil {
			return nil, fmt.Errorf("listing pools in cloudspace %s: %w", cs, err)
		}
		for _, p := range pools {
			claims = append(claims, c.poolToClaim(ctx, p))
		}
	}
	return claims, nil
}

func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*karpcloudprovider.InstanceType, error) {
	if nodePool == nil || nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, nil
	}
	var nodeClass apiv1.RackspaceSpotNodeClass
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, &nodeClass); err != nil {
		return nil, fmt.Errorf("getting NodeClass %q: %w", nodePool.Spec.Template.Spec.NodeClassRef.Name, err)
	}
	region, err := c.cloudspaceRegion(ctx, &nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving cloudspace region: %w", err)
	}
	return c.instanceType.List(ctx, region)
}

// ---- helpers ----

func (c *CloudProvider) resolveNodeClass(ctx context.Context, nc *karpv1.NodeClaim) (*apiv1.RackspaceSpotNodeClass, error) {
	if nc.Spec.NodeClassRef == nil {
		return nil, errors.New("NodeClaim has no nodeClassRef")
	}
	if nc.Spec.NodeClassRef.Kind != NodeClassKind {
		return nil, fmt.Errorf("nodeClassRef.kind %q does not match %q", nc.Spec.NodeClassRef.Kind, NodeClassKind)
	}
	var nodeClass apiv1.RackspaceSpotNodeClass
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nc.Spec.NodeClassRef.Name}, &nodeClass); err != nil {
		return nil, fmt.Errorf("getting RackspaceSpotNodeClass %q: %w", nc.Spec.NodeClassRef.Name, err)
	}
	return &nodeClass, nil
}

// cloudspaceRegion returns the region of the Cloudspace referenced by the
// NodeClass, preferring the cached value in NodeClass.Status.Region and
// falling back to a live SDK lookup.
func (c *CloudProvider) cloudspaceRegion(ctx context.Context, nc *apiv1.RackspaceSpotNodeClass) (string, error) {
	if nc.Status.Region != "" {
		return nc.Status.Region, nil
	}
	if nc.Spec.CloudspaceName == "" {
		return "", errors.New("NodeClass has no cloudspaceName")
	}
	org, err := c.instances.OrganizationID(ctx)
	if err != nil {
		return "", err
	}
	cs, err := c.spotAPI.GetCloudspace(ctx, org, nc.Spec.CloudspaceName)
	if err != nil {
		return "", fmt.Errorf("getting cloudspace %s: %w", nc.Spec.CloudspaceName, err)
	}
	if cs.Region == "" {
		return "", fmt.Errorf("cloudspace %s has no region", nc.Spec.CloudspaceName)
	}
	return cs.Region, nil
}

func (c *CloudProvider) pickInstanceType(ctx context.Context, nc *karpv1.NodeClaim, region string) (*karpcloudprovider.InstanceType, string, error) {
	instanceTypeName := requirementValue(nc, corev1.LabelInstanceTypeStable)
	if instanceTypeName == "" {
		return nil, "", errors.New("NodeClaim requirements do not pin an instance type")
	}
	capacityType := requirementValue(nc, karpv1.CapacityTypeLabelKey)
	if capacityType == "" {
		capacityType = karpv1.CapacityTypeOnDemand
	}
	it, err := c.instanceType.Get(ctx, region, instanceTypeName)
	if err != nil {
		return nil, "", fmt.Errorf("looking up instance type %q in region %s: %w", instanceTypeName, region, err)
	}
	return it, capacityType, nil
}

func (c *CloudProvider) hydrateClaim(orig *karpv1.NodeClaim, pool *instance.Pool, it *karpcloudprovider.InstanceType, capacityType, region string) *karpv1.NodeClaim {
	out := orig.DeepCopy()
	out.Status.ProviderID = pool.ProviderID
	out.Status.Capacity = it.Capacity
	out.Status.Allocatable = it.Allocatable()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Labels[corev1.LabelInstanceTypeStable] = it.Name
	out.Labels[karpv1.CapacityTypeLabelKey] = capacityType
	out.Labels[corev1.LabelTopologyZone] = region
	out.Labels[corev1.LabelTopologyRegion] = region
	out.Labels[corev1.LabelArchStable] = karpv1.ArchitectureAmd64
	out.Labels[corev1.LabelOSStable] = string(corev1.Linux)
	return out
}

// poolToClaim materializes a minimal NodeClaim from an existing pool. Capacity
// and Allocatable are populated only when we can cheaply look up the
// InstanceType (best-effort — we don't fail Get/List if pricing/instance-type
// cache lookups fall over).
func (c *CloudProvider) poolToClaim(ctx context.Context, p *instance.Pool) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{}
	nc.Name = p.Labels[instance.NodeClaimNameLabel]
	nc.UID = types.UID(p.Labels[instance.NodeClaimUIDLabel])
	nc.Labels = map[string]string{
		corev1.LabelInstanceTypeStable: p.ServerClass,
		karpv1.CapacityTypeLabelKey:    p.CapacityType,
	}
	nc.Status.ProviderID = p.ProviderID
	return nc
}

func requirementValue(nc *karpv1.NodeClaim, key string) string {
	for _, r := range nc.Spec.Requirements {
		if r.Key == key && len(r.Values) > 0 {
			return r.Values[0]
		}
	}
	return ""
}
