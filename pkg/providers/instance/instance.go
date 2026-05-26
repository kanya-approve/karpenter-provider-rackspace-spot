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
	"fmt"
	"strings"
	"sync"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
)

const (
	Scheme = "rackspacespot"

	PoolTypeSpot     = "spot"
	PoolTypeOnDemand = "ondemand"

	// KarpenterManagedLabel is set on every pool we create; List() filters
	// foreign pools out by this label. Rackspace's admission webhook
	// requires pool names to be a bare lowercase UUID, so we can't use a
	// name prefix to mark ours.
	KarpenterManagedLabel = "karpenter.rackspace.com/managed"
	NodeClaimNameLabel    = "karpenter.rackspace.com/nodeclaim-name"
	NodeClaimUIDLabel     = "karpenter.rackspace.com/nodeclaim-uid"
)

// ErrPoolNotFound is returned by Get/Delete when the pool no longer exists.
var ErrPoolNotFound = errors.New("rackspace spot pool not found")

// Pool is the provider's normalized view of a Rackspace SpotNodePool or
// OnDemandNodePool.
type Pool struct {
	Name         string
	Type         string // PoolTypeSpot | PoolTypeOnDemand
	Cloudspace   string
	ServerClass  string
	BidPrice     string
	Desired      int
	WonCount     int
	CapacityType string // karpv1.CapacityType*
	Labels       map[string]string
	Annotations  map[string]string
	Status       string
	ProviderID   string
}

type Provider interface {
	Create(ctx context.Context, nodeClass *apiv1.RackspaceSpotNodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*karpcloudprovider.InstanceType) (*Pool, error)
	Get(ctx context.Context, providerID string) (*Pool, error)
	Delete(ctx context.Context, providerID string) error
	List(ctx context.Context, cloudspace string) ([]*Pool, error)
	// OrganizationID returns the (cached) Rackspace org ID derived from the
	// authenticated principal. Other packages need it to call Cloudspace and
	// other org-scoped SDK endpoints.
	OrganizationID(ctx context.Context) (string, error)
}

// API is the narrow subset of the rxtspot SDK the instance provider depends
// on. The full rxtspot.SpotAPI satisfies it, and tests can compose the
// per-resource mocks (gomock fixtures the SDK ships) to satisfy it without
// pulling in the broken MockSpotAPI super-mock.
type API interface {
	rxtspot.SpotNodePoolAPI
	rxtspot.OnDemandNodePoolAPI
	rxtspot.OrganizationAPI
}

type DefaultProvider struct {
	spot     rxtspot.SpotNodePoolAPI
	onDemand rxtspot.OnDemandNodePoolAPI
	orgs     rxtspot.OrganizationAPI

	orgMu sync.Mutex
	org   string
}

func NewProvider(api API) *DefaultProvider {
	return &DefaultProvider{spot: api, onDemand: api, orgs: api}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *apiv1.RackspaceSpotNodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*karpcloudprovider.InstanceType) (*Pool, error) {
	if len(instanceTypes) == 0 {
		return nil, errors.New("no instance types provided")
	}
	instanceType := instanceTypes[0]
	capacityType := deriveCapacityType(nodeClaim)

	org, err := p.organization(ctx)
	if err != nil {
		return nil, err
	}

	name := PoolName(nodeClaim)
	cloudspace := nodeClass.Spec.CloudspaceName
	serverClass := instanceType.Name
	labels := mergeLabels(nodeClass.Spec.Labels, nodeClaim)
	taints := convertTaints(nodeClass.Spec.Taints)

	switch capacityType {
	case karpv1.CapacityTypeSpot:
		if nodeClass.Spec.BidPrice == "" {
			return nil, fmt.Errorf("bid price required for spot pool %s", name)
		}
		pool := rxtspot.SpotNodePool{
			Name:              name,
			Org:               org,
			Cloudspace:        cloudspace,
			ServerClass:       serverClass,
			Desired:           1,
			BidPrice:          nodeClass.Spec.BidPrice,
			CustomLabels:      labels,
			CustomAnnotations: nodeClass.Spec.Annotations,
			CustomTaints:      taints,
		}
		if err := p.spot.CreateSpotNodePool(ctx, org, pool); err != nil && !isAlreadyExists(err) {
			return nil, fmt.Errorf("creating spot pool %s: %w", name, err)
		}
		return p.Get(ctx, MakeProviderID(cloudspace, PoolTypeSpot, name))

	case karpv1.CapacityTypeOnDemand:
		pool := rxtspot.OnDemandNodePool{
			Name:              name,
			Org:               org,
			Cloudspace:        cloudspace,
			ServerClass:       serverClass,
			Desired:           1,
			CustomLabels:      labels,
			CustomAnnotations: nodeClass.Spec.Annotations,
			CustomTaints:      taints,
		}
		if err := p.onDemand.CreateOnDemandNodePool(ctx, org, pool); err != nil && !isAlreadyExists(err) {
			return nil, fmt.Errorf("creating on-demand pool %s: %w", name, err)
		}
		return p.Get(ctx, MakeProviderID(cloudspace, PoolTypeOnDemand, name))

	default:
		return nil, fmt.Errorf("unsupported capacity type %q", capacityType)
	}
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Pool, error) {
	cs, kind, name, err := ParseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	org, err := p.organization(ctx)
	if err != nil {
		return nil, err
	}
	switch kind {
	case PoolTypeSpot:
		sp, err := p.spot.GetSpotNodePool(ctx, org, name)
		if err != nil {
			if isNotFound(err) {
				return nil, ErrPoolNotFound
			}
			return nil, fmt.Errorf("getting spot pool %s: %w", name, err)
		}
		return spotToPool(sp, cs), nil
	case PoolTypeOnDemand:
		od, err := p.onDemand.GetOnDemandNodePool(ctx, org, name)
		if err != nil {
			if isNotFound(err) {
				return nil, ErrPoolNotFound
			}
			return nil, fmt.Errorf("getting on-demand pool %s: %w", name, err)
		}
		return onDemandToPool(od, cs), nil
	}
	return nil, fmt.Errorf("unknown pool kind %q in provider ID %q", kind, providerID)
}

func (p *DefaultProvider) Delete(ctx context.Context, providerID string) error {
	_, kind, name, err := ParseProviderID(providerID)
	if err != nil {
		return err
	}
	org, err := p.organization(ctx)
	if err != nil {
		return err
	}
	switch kind {
	case PoolTypeSpot:
		if err := p.spot.DeleteSpotNodePool(ctx, org, name); err != nil {
			if isNotFound(err) {
				return ErrPoolNotFound
			}
			return fmt.Errorf("deleting spot pool %s: %w", name, err)
		}
		return nil
	case PoolTypeOnDemand:
		if err := p.onDemand.DeleteOnDemandNodePool(ctx, org, name); err != nil {
			if isNotFound(err) {
				return ErrPoolNotFound
			}
			return fmt.Errorf("deleting on-demand pool %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("unknown pool kind %q in provider ID %q", kind, providerID)
}

func (p *DefaultProvider) List(ctx context.Context, cloudspace string) ([]*Pool, error) {
	org, err := p.organization(ctx)
	if err != nil {
		return nil, err
	}
	spots, err := p.spot.ListSpotNodePools(ctx, org, cloudspace)
	if err != nil {
		return nil, fmt.Errorf("listing spot pools: %w", err)
	}
	ods, err := p.onDemand.ListOnDemandNodePools(ctx, org, cloudspace)
	if err != nil {
		return nil, fmt.Errorf("listing on-demand pools: %w", err)
	}

	pools := make([]*Pool, 0, len(spots)+len(ods))
	for _, sp := range spots {
		if sp.CustomLabels[KarpenterManagedLabel] != "true" {
			continue
		}
		pools = append(pools, spotToPool(sp, cloudspace))
	}
	for _, od := range ods {
		if od.CustomLabels[KarpenterManagedLabel] != "true" {
			continue
		}
		pools = append(pools, onDemandToPool(od, cloudspace))
	}
	return pools, nil
}

func (p *DefaultProvider) OrganizationID(ctx context.Context) (string, error) {
	return p.organization(ctx)
}

func (p *DefaultProvider) organization(ctx context.Context) (string, error) {
	p.orgMu.Lock()
	defer p.orgMu.Unlock()
	if p.org != "" {
		return p.org, nil
	}
	orgs, err := p.orgs.ListOrganizations(ctx)
	if err != nil {
		return "", fmt.Errorf("listing organizations: %w", err)
	}
	if len(orgs) == 0 {
		return "", errors.New("no organizations available for authenticated user")
	}
	p.org = orgs[0].ID
	return p.org, nil
}

// PoolName returns the deterministic Rackspace pool name for a NodeClaim.
// PoolName returns the NodeClaim's UID verbatim. Rackspace's admission
// webhook requires every SpotNodePool / OnDemandNodePool name to be a
// lowercase UUID; k8s NodeClaim UIDs already satisfy that, so we use them
// as-is.
func PoolName(nc *karpv1.NodeClaim) string {
	return string(nc.UID)
}

// deriveCapacityType picks one capacity type from the NodeClaim's
// karpenter.sh/capacity-type requirement. Defaults to on-demand when unset.
// If multiple values are allowed (e.g. ["spot","on-demand"]), the first wins —
// the scheduler ordered them by preference.
func deriveCapacityType(nc *karpv1.NodeClaim) string {
	for _, r := range nc.Spec.Requirements {
		if r.Key == karpv1.CapacityTypeLabelKey && len(r.Values) > 0 {
			return r.Values[0]
		}
	}
	return karpv1.CapacityTypeOnDemand
}

// MakeProviderID builds a Karpenter providerID for a Rackspace pool.
// Shape: rackspacespot://<cloudspace>/<kind>/<pool-name>
func MakeProviderID(cloudspace, kind, name string) string {
	return fmt.Sprintf("%s://%s/%s/%s", Scheme, cloudspace, kind, name)
}

// ParseProviderID is the inverse of MakeProviderID.
func ParseProviderID(id string) (cloudspace, kind, name string, err error) {
	prefix := Scheme + "://"
	if !strings.HasPrefix(id, prefix) {
		return "", "", "", fmt.Errorf("invalid provider ID %q (must start with %s)", id, prefix)
	}
	parts := strings.SplitN(strings.TrimPrefix(id, prefix), "/", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid provider ID %q (expected cloudspace/kind/name)", id)
	}
	return parts[0], parts[1], parts[2], nil
}

func spotToPool(sp *rxtspot.SpotNodePool, cloudspace string) *Pool {
	return &Pool{
		Name:         sp.Name,
		Type:         PoolTypeSpot,
		Cloudspace:   sp.Cloudspace,
		ServerClass:  sp.ServerClass,
		BidPrice:     sp.BidPrice,
		Desired:      sp.Desired,
		WonCount:     sp.WonCount,
		CapacityType: karpv1.CapacityTypeSpot,
		Labels:       sp.CustomLabels,
		Annotations:  sp.CustomAnnotations,
		Status:       sp.Status,
		ProviderID:   MakeProviderID(cloudspace, PoolTypeSpot, sp.Name),
	}
}

func onDemandToPool(od *rxtspot.OnDemandNodePool, cloudspace string) *Pool {
	return &Pool{
		Name:         od.Name,
		Type:         PoolTypeOnDemand,
		Cloudspace:   od.Cloudspace,
		ServerClass:  od.ServerClass,
		Desired:      od.Desired,
		WonCount:     od.WonCount,
		CapacityType: karpv1.CapacityTypeOnDemand,
		Labels:       od.CustomLabels,
		Annotations:  od.CustomAnnotations,
		Status:       od.Status,
		ProviderID:   MakeProviderID(cloudspace, PoolTypeOnDemand, od.Name),
	}
}

func mergeLabels(extra map[string]string, nc *karpv1.NodeClaim) map[string]string {
	out := map[string]string{
		KarpenterManagedLabel: "true",
		NodeClaimNameLabel:    nc.Name,
		NodeClaimUIDLabel:     string(nc.UID),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func convertTaints(taints []corev1.Taint) []interface{} {
	out := make([]interface{}, 0, len(taints))
	for _, t := range taints {
		out = append(out, map[string]interface{}{
			"key":    t.Key,
			"value":  t.Value,
			"effect": string(t.Effect),
		})
	}
	return out
}

// isAlreadyExists / isNotFound do best-effort error classification. The SDK
// returns wrapped HTTPStatusError without exposing a typed sentinel, so we
// inspect the message. See GH issue #4 (rate-limit / quota work) for the
// follow-up to wire structured error handling end-to-end.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "AlreadyExists") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, " 409 ")
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NotFound") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, " 404 ")
}
