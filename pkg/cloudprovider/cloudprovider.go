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
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

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
	cloudspace   string
	region       string
}

var _ karpcloudprovider.CloudProvider = (*CloudProvider)(nil)

func New(op *operator.Operator) *CloudProvider {
	return &CloudProvider{
		kubeClient:   op.GetClient(),
		instances:    op.InstanceProvider,
		instanceType: op.InstanceTypeProvider,
		cloudspace:   op.CloudspaceName,
		region:       op.Region,
	}
}

func (c *CloudProvider) Name() string { return Name }

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&apiv1.RackspaceSpotNodeClass{}}
}

// IsDrifted is a no-op for this provider. Rackspace pools have no
// node-immutable spec fields worth a roll: the Cloudspace is fixed per
// operator deploy, Labels/Annotations/Taints can be mutated in-place on
// the Node, and the bid is locked at pool create time. If a real drift
// surface emerges later, implement it then.
func (c *CloudProvider) IsDrifted(_ context.Context, _ *karpv1.NodeClaim) (karpcloudprovider.DriftReason, error) {
	return "", nil
}

// RepairPolicies returns the standard 30-min toleration window for
// Ready=False/Unknown and NetworkUnavailable=True. 30 min matches the
// AWS/Azure/OCI convention — rides out brief blips, doesn't let a wedged
// node strand workloads for hours.
func (c *CloudProvider) RepairPolicies() []karpcloudprovider.RepairPolicy {
	return []karpcloudprovider.RepairPolicy{
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute},
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, TolerationDuration: 30 * time.Minute},
		{ConditionType: corev1.NodeNetworkUnavailable, ConditionStatus: corev1.ConditionTrue, TolerationDuration: 30 * time.Minute},
	}
}

func (c *CloudProvider) Create(ctx context.Context, nc *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.resolveNodeClass(ctx, nc)
	if err != nil {
		return nil, fmt.Errorf("resolving node class: %w", err)
	}

	instType, capacityType, err := c.pickInstanceType(ctx, nc, c.region)
	if err != nil {
		return nil, err
	}

	pool, err := c.instances.Create(ctx, nodeClass, nc, []*karpcloudprovider.InstanceType{instType})
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	return c.hydrateClaim(nc, pool, instType, capacityType, c.region), nil
}

func (c *CloudProvider) Delete(ctx context.Context, nc *karpv1.NodeClaim) error {
	if nc.UID == "" {
		return karpcloudprovider.NewNodeClaimNotFoundError(errors.New("nodeclaim has no UID"))
	}
	// Pool name is the NodeClaim UID. We don't rely on parsing Status.ProviderID
	// because nodelink may have rewritten it to the Rackspace CCM's
	// openstack:///<vm-uuid> form, which doesn't carry pool kind/name.
	poolName := string(nc.UID)
	capacityType := requirementValue(nc, karpv1.CapacityTypeLabelKey)
	kind := instance.PoolTypeSpot
	if capacityType == karpv1.CapacityTypeOnDemand {
		kind = instance.PoolTypeOnDemand
	}
	if err := c.instances.Delete(ctx, instance.MakeProviderID(c.cloudspace, kind, poolName)); err != nil {
		if errors.Is(err, instance.ErrPoolNotFound) {
			return karpcloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}
	return nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	// Synthetic scheme path — directly resolvable via pool API.
	if strings.HasPrefix(providerID, instance.Scheme+"://") {
		pool, err := c.instances.Get(ctx, providerID)
		if err != nil {
			if errors.Is(err, instance.ErrPoolNotFound) {
				return nil, karpcloudprovider.NewNodeClaimNotFoundError(err)
			}
			return nil, err
		}
		return c.poolToClaim(ctx, pool), nil
	}
	// CCM-set scheme (openstack:///<uuid>): find the Node carrying that
	// providerID + our managed label, then look up its pool.
	var nodes corev1.NodeList
	if err := c.kubeClient.List(ctx, &nodes, client.MatchingLabels{instance.KarpenterManagedLabel: "true"}); err != nil {
		return nil, fmt.Errorf("listing managed nodes: %w", err)
	}
	for i := range nodes.Items {
		if nodes.Items[i].Spec.ProviderID != providerID {
			continue
		}
		nodeClaimUID := nodes.Items[i].Labels[instance.NodeClaimUIDLabel]
		capacityType := nodes.Items[i].Labels[karpv1.CapacityTypeLabelKey]
		kind := instance.PoolTypeSpot
		if capacityType == karpv1.CapacityTypeOnDemand {
			kind = instance.PoolTypeOnDemand
		}
		pool, err := c.instances.Get(ctx, instance.MakeProviderID(c.cloudspace, kind, nodeClaimUID))
		if err != nil {
			if errors.Is(err, instance.ErrPoolNotFound) {
				return nil, karpcloudprovider.NewNodeClaimNotFoundError(err)
			}
			return nil, err
		}
		nc := c.poolToClaim(ctx, pool)
		nc.Status.ProviderID = providerID // preserve the openstack-form expected by Karpenter
		return nc, nil
	}
	return nil, karpcloudprovider.NewNodeClaimNotFoundError(errors.New("no managed Node with that providerID"))
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	pools, err := c.instances.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing pools in cloudspace %s: %w", c.cloudspace, err)
	}

	// Index managed Nodes by NodeClaim UID so we can substitute their
	// CCM-set providerID into the NodeClaim we return — otherwise
	// Karpenter's cluster-state sync compares our synthetic scheme to
	// the rewritten Status.ProviderID and stalls.
	var nodes corev1.NodeList
	if err := c.kubeClient.List(ctx, &nodes, client.MatchingLabels{instance.KarpenterManagedLabel: "true"}); err != nil {
		return nil, fmt.Errorf("listing managed nodes: %w", err)
	}
	nodeByUID := map[string]*corev1.Node{}
	for i := range nodes.Items {
		if uid := nodes.Items[i].Labels[instance.NodeClaimUIDLabel]; uid != "" {
			nodeByUID[uid] = &nodes.Items[i]
		}
	}

	claims := make([]*karpv1.NodeClaim, 0, len(pools))
	for _, p := range pools {
		nc := c.poolToClaim(ctx, p)
		if node, ok := nodeByUID[p.Labels[instance.NodeClaimUIDLabel]]; ok && node.Spec.ProviderID != "" {
			nc.Status.ProviderID = node.Spec.ProviderID
		}
		claims = append(claims, nc)
	}
	return claims, nil
}

func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*karpcloudprovider.InstanceType, error) {
	if nodePool == nil || nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, nil
	}
	return c.instanceType.List(ctx, c.region)
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

// pickInstanceType selects the cheapest available ServerClass compatible with
// the NodeClaim's requirements for its capacity type. Karpenter passes the full
// set of resource-feasible instance types in the NodeClaim's
// node.kubernetes.io/instance-type requirement; launching the cheapest of them
// — rather than an arbitrary one — is the cost optimization Karpenter expects a
// cloud provider to perform.
func (c *CloudProvider) pickInstanceType(ctx context.Context, nc *karpv1.NodeClaim, region string) (*karpcloudprovider.InstanceType, string, error) {
	capacityType := requirementValue(nc, karpv1.CapacityTypeLabelKey)
	if capacityType == "" {
		capacityType = karpv1.CapacityTypeOnDemand
	}

	instanceTypes, err := c.instanceType.List(ctx, region)
	if err != nil {
		return nil, "", fmt.Errorf("listing instance types in region %s: %w", region, err)
	}

	// Constrain the NodeClaim's requirements to the single capacity type we will
	// create the pool with, so the price we compare is the price we will pay.
	// instance.Create derives the same capacity type independently.
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nc.Spec.Requirements...)
	reqs.Add(scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityType))

	var best *karpcloudprovider.InstanceType
	var bestPrice float64
	for _, it := range instanceTypes {
		// The instance type's own labels (name, arch, os, zone) must satisfy the
		// NodeClaim; Intersects mirrors core's instance-type filter.
		if it.Requirements.Intersects(reqs) != nil {
			continue
		}
		offering := it.Offerings.Available().Compatible(reqs).Cheapest()
		if offering == nil {
			continue
		}
		if best == nil || offering.Price < bestPrice {
			best, bestPrice = it, offering.Price
		}
	}
	if best == nil {
		return nil, "", fmt.Errorf("no available instance type compatible with NodeClaim requirements (capacity type %q)", capacityType)
	}
	return best, capacityType, nil
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
