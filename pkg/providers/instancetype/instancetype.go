/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package instancetype

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	resourceNvidiaGPU = "nvidia.com/gpu"
	defaultPodsPerNode = 110
)

// Provider serves Karpenter cloudprovider.InstanceType objects translated from
// Rackspace ServerClass records, with results cached per region.
type Provider interface {
	List(ctx context.Context, region string) ([]*karpcloudprovider.InstanceType, error)
	Get(ctx context.Context, region, name string) (*karpcloudprovider.InstanceType, error)
	// MinBidPrice returns the ServerClass-specific bid floor Rackspace's
	// admission webhook enforces. Pulled from the cached SDK ServerClass
	// (MinBidPricePerHour), not the percentile feed (which doesn't carry it).
	MinBidPrice(ctx context.Context, region, name string) (float64, error)
}

type DefaultProvider struct {
	api          rxtspot.SpotServerClassesAPI
	refreshAfter time.Duration

	mu    sync.Mutex
	cache map[string]regionCache
}

type regionCache struct {
	classes []rxtspot.ServerClass
	fetched time.Time
}

func NewProvider(api rxtspot.SpotServerClassesAPI) *DefaultProvider {
	return &DefaultProvider{
		api:          api,
		refreshAfter: 5 * time.Minute,
		cache:        map[string]regionCache{},
	}
}

func (p *DefaultProvider) List(ctx context.Context, region string) ([]*karpcloudprovider.InstanceType, error) {
	classes, err := p.classes(ctx, region)
	if err != nil {
		return nil, err
	}
	return lo.Map(classes, func(sc rxtspot.ServerClass, _ int) *karpcloudprovider.InstanceType {
		return translate(sc)
	}), nil
}

func (p *DefaultProvider) Get(ctx context.Context, region, name string) (*karpcloudprovider.InstanceType, error) {
	classes, err := p.classes(ctx, region)
	if err != nil {
		return nil, err
	}
	sc, found := lo.Find(classes, func(s rxtspot.ServerClass) bool { return s.Name == name })
	if !found {
		return nil, fmt.Errorf("server class %q not found in region %q", name, region)
	}
	return translate(sc), nil
}

func (p *DefaultProvider) MinBidPrice(ctx context.Context, region, name string) (float64, error) {
	classes, err := p.classes(ctx, region)
	if err != nil {
		return 0, err
	}
	sc, found := lo.Find(classes, func(s rxtspot.ServerClass) bool { return s.Name == name })
	if !found {
		return 0, fmt.Errorf("server class %q not found in region %q", name, region)
	}
	return parsePrice(sc.MinBidPricePerHour), nil
}

func (p *DefaultProvider) classes(ctx context.Context, region string) ([]rxtspot.ServerClass, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[region]; ok && time.Since(c.fetched) < p.refreshAfter {
		return c.classes, nil
	}
	list, err := p.api.ListServerClasses(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("listing server classes in %s: %w", region, err)
	}
	p.cache[region] = regionCache{classes: list.Items, fetched: time.Now()}
	return list.Items, nil
}

func translate(sc rxtspot.ServerClass) *karpcloudprovider.InstanceType {
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    parseQuantity(sc.Resources.CPU),
		corev1.ResourceMemory: parseQuantity(sc.Resources.Memory),
		corev1.ResourcePods:   *resource.NewQuantity(defaultPodsPerNode, resource.DecimalSI),
	}
	if gpu := parseQuantity(sc.Resources.GPU); !gpu.IsZero() {
		capacity[resourceNvidiaGPU] = gpu
	}

	zone := sc.Region // Rackspace Cloudspaces have no AZs; treat region as the single zone.
	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, sc.Name),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, sc.Region),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, karpv1.ArchitectureAmd64),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
	)

	available := sc.Availability != "unavailable" && sc.Availability != ""

	var offerings karpcloudprovider.Offerings
	if onDemand := parsePrice(sc.OnDemandPricePerHour); onDemand > 0 {
		offerings = append(offerings, &karpcloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
			),
			Price:     onDemand,
			Available: available,
		})
	}
	if spot := parsePrice(sc.CurrentMarketPricePerHour); spot > 0 {
		offerings = append(offerings, &karpcloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
			),
			Price:     spot,
			Available: available,
		})
	}

	return &karpcloudprovider.InstanceType{
		Name:         sc.Name,
		Requirements: requirements,
		Offerings:    offerings,
		Capacity:     capacity,
		Overhead: &karpcloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		},
	}
}

// rackspaceUnitFix normalizes Rackspace's "<n>GB"/"<n>MB"/"<n>TB"/"<n>KB"
// suffixes to k8s canonical Gi/Mi/Ti/Ki so resource.ParseQuantity accepts
// them. Rackspace's API returns memory as e.g. "3.75GB" which K8s rejects.
func rackspaceUnitFix(s string) string {
	for _, suf := range []string{"GB", "MB", "TB", "KB", "PB", "EB"} {
		if strings.HasSuffix(s, suf) {
			return strings.TrimSuffix(s, suf) + suf[:1] + "i"
		}
	}
	return s
}

func parseQuantity(s string) resource.Quantity {
	if s == "" {
		return resource.Quantity{}
	}
	q, err := resource.ParseQuantity(rackspaceUnitFix(s))
	if err != nil {
		return resource.Quantity{}
	}
	return q
}

// parsePrice handles Rackspace's "$0.001000"-style strings (currency prefix
// + leading/trailing whitespace) and returns 0 when the value can't be parsed.
func parsePrice(s string) float64 {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
