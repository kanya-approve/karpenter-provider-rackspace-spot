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
	"math"
	"strconv"
	"strings"
	"sync"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/pricing"
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
	spot         rxtspot.SpotNodePoolAPI
	onDemand     rxtspot.OnDemandNodePoolAPI
	orgs         rxtspot.OrganizationAPI
	pricing      pricing.Provider
	instanceType instancetype.Provider

	orgMu sync.Mutex
	org   string
}

func NewProvider(api API, pricingProvider pricing.Provider, instanceTypeProvider instancetype.Provider) *DefaultProvider {
	return &DefaultProvider{spot: api, onDemand: api, orgs: api, pricing: pricingProvider, instanceType: instanceTypeProvider}
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
	labels := mergeLabels(nodeClass, nodeClaim, instanceType, capacityType)
	taints := convertTaints(nodeClass.Spec.Taints)

	switch capacityType {
	case karpv1.CapacityTypeSpot:
		bid, err := p.chooseBidPrice(ctx, nodeClass, instanceType)
		if err != nil {
			return nil, fmt.Errorf("choosing bid for spot pool %s: %w", name, err)
		}
		pool := rxtspot.SpotNodePool{
			Name:              name,
			Org:               org,
			Cloudspace:        cloudspace,
			ServerClass:       serverClass,
			Desired:           1,
			BidPrice:          bid,
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

// chooseBidPrice computes the bid we send to Rackspace at pool-create time.
//
// Strategy: bid max(market, percentile_from_NodeClass) * 1.05.
//   - market clears Rackspace's "bid >= current market" admission check.
//   - percentile_from_NodeClass is one of P20/P50/P80 (default P80) from
//     Rackspace's published 30-day price distribution; bidding at-or-above
//     that level means we hold the node through that fraction of typical
//     price ticks rather than getting reclaimed.
//   - * 1.05 adds a small buffer for intra-tick movement.
//
// Falls back to market * 1.2 if the percentile feed lookup fails for any
// reason (cold start before first live fetch, region/SC missing, etc.).
func (p *DefaultProvider) chooseBidPrice(ctx context.Context, nodeClass *apiv1.RackspaceSpotNodeClass, instanceType *karpcloudprovider.InstanceType) (string, error) {
	var marketPrice float64
	for _, off := range instanceType.Offerings {
		if off.Requirements.Get(karpv1.CapacityTypeLabelKey).Any() == karpv1.CapacityTypeSpot {
			marketPrice = off.Price
			break
		}
	}
	if marketPrice == 0 {
		return "", fmt.Errorf("no spot offering or zero price for instance type %q", instanceType.Name)
	}

	region := instanceType.Requirements.Get(corev1.LabelTopologyZone).Any()
	target := marketPrice * 1.2 // fallback if percentile lookup fails
	if region != "" && p.pricing != nil {
		if pct, err := p.pricing.Percentiles(ctx, region, instanceType.Name); err == nil {
			pivot := marketPrice
			if chosen := selectPercentile(nodeClass.Spec.BidPercentile, pct); chosen > pivot {
				pivot = chosen
			}
			target = pivot * 1.05
		} else {
			log.FromContext(ctx).V(1).Info("percentile lookup failed, falling back to market*1.2", "err", err.Error(), "instanceType", instanceType.Name)
		}
	}

	// Rackspace enforces a per-ServerClass bid FLOOR via MinBidPricePerHour.
	// If our market+percentile computation falls below it, the admission
	// webhook rejects with "BidPrice must be greater than or equal to the
	// minimum bid price of X". Clamp up.
	if region != "" && p.instanceType != nil {
		if minBid, err := p.instanceType.MinBidPrice(ctx, region, instanceType.Name); err == nil && target < minBid {
			target = minBid
		}
	}
	return strconv.FormatFloat(roundBidUp(target), 'f', -1, 64), nil
}

// selectPercentile picks one of the published percentile values per the
// NodeClass's BidPercentile field. Empty (or unrecognized) defaults to P80,
// matching the CRD default.
func selectPercentile(choice string, p pricing.Percentiles) float64 {
	switch choice {
	case "P20":
		return p.P20
	case "P50":
		return p.P50
	case "", "P80":
		return p.P80
	default:
		return p.P80
	}
}

// roundBidUp honors Rackspace's admission validation:
//   - "bidPrice can only be a positive number up to three decimal places"
//   - "BidPrice must be a multiple of 0.01 when greater than 0.05"
//
// Rounding is always UP so we never accidentally fall below the market
// price we just computed against.
func roundBidUp(bid float64) float64 {
	if bid > 0.05 {
		return math.Ceil(bid*100) / 100 // 2 dp (multiples of 0.01)
	}
	return math.Ceil(bid*1000) / 1000 // 3 dp
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

// mergeLabels builds the CustomLabels passed to Rackspace's pool API. These
// flow through to kubelet --node-labels on the joining Node.
//
// We deliberately do NOT set node.kubernetes.io/instance-type or
// topology.kubernetes.io/region: Rackspace's OpenStack CCM owns those
// post-registration (setting them to e.g. "compute1-4" / "HKG"), and the
// pool API surfaces a "Custom Metadata conflicts" warning when we try.
// karpenter.sh/* and karpenter.rackspace.com/* are untouched by the CCM,
// so those are sufficient for Karpenter binding + our internal tracking.
// topology.kubernetes.io/zone survives the CCM and is required by the
// scheduler, so we keep it. See GH issue #9.
func mergeLabels(nodeClass *apiv1.RackspaceSpotNodeClass, nc *karpv1.NodeClaim, instanceType *karpcloudprovider.InstanceType, capacityType string) map[string]string {
	zone := instanceType.Requirements.Get(corev1.LabelTopologyZone).Any()
	out := map[string]string{
		KarpenterManagedLabel:       "true",
		NodeClaimNameLabel:          nc.Name,
		NodeClaimUIDLabel:           string(nc.UID),
		karpv1.CapacityTypeLabelKey: capacityType,
		corev1.LabelTopologyZone:    zone,
	}
	if np := nc.Labels[karpv1.NodePoolLabelKey]; np != "" {
		out[karpv1.NodePoolLabelKey] = np
	}
	for k, v := range nodeClass.Spec.Labels {
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
	// We deliberately do NOT append karpenter.sh/unregistered:NoExecute here.
	// Rackspace's pool controller stamps customTaints onto each node once at
	// creation, but it runs after Karpenter's registration loop. The result
	// was a stuck taint Karpenter never got the chance to remove. Karpenter
	// logs UnregisteredTaintMissingEvent at registration, which is cosmetic.
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
