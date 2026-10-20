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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	resourceNvidiaGPU  = "nvidia.com/gpu"
	defaultPodsPerNode = 110
	// Floor for classes whose /disk the API omits (bare metal, deprecated micro).
	defaultEphemeralStorageGi = 40
	serverClassesPath         = "/apis/ngpc.rxt.io/v1/serverclasses"
)

// Publishing raw ServerClass numbers overstates what the kubelet offers, so the
// scheduler nominates pods onto nodes that cannot hold them and the provisioner
// relaunches the same too-small flavor forever. Reservations below are
// kubeReserved+systemReserved and the memory/nodefs evictionHard thresholds from
// the node kubelet config (/proxy/configz), identical on every flavor. The
// percentages cover the separate gap between the ServerClass size and what the
// booted VM reports, worst case observed 2.5% on memory and 3.3% on disk.
const (
	defaultKubeReservedCPU              = "500m"
	defaultKubeReservedMemory           = "1024Mi"
	defaultKubeReservedEphemeralStorage = "2Gi"
	defaultMemoryEvictionThreshold      = "100Mi"
	defaultDiskEvictionPercent          = 0.10
	defaultVMMemoryOverheadPercent      = 0.03
	defaultVMDiskOverheadPercent        = 0.04

	envKubeReservedCPU    = "RACKSPACE_KUBE_RESERVED_CPU"
	envKubeReservedMemory = "RACKSPACE_KUBE_RESERVED_MEMORY"
	envVMMemoryOverhead   = "VM_MEMORY_OVERHEAD_PERCENT"
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
	// UpdateFromNode keeps the smallest capacity ever reported for an instance
	// type, so one node that boots with less than its predecessors ratchets the
	// estimate down and nothing raises it back up.
	UpdateFromNode(instanceType string, capacity corev1.ResourceList)
}

// CPU and pods are advertised exactly, so only these two are worth measuring.
var discoveredResources = []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceEphemeralStorage}

type DefaultProvider struct {
	client       *rxtspot.RackspaceSpotClient
	refreshAfter time.Duration

	mu         sync.Mutex
	cache      map[string]regionCache
	discovered map[string]corev1.ResourceList
}

type regionCache struct {
	classes []rxtspot.ServerClass
	disks   map[string]string
	fetched time.Time
}

func NewProvider(client *rxtspot.RackspaceSpotClient) *DefaultProvider {
	return &DefaultProvider{
		client:       client,
		refreshAfter: 5 * time.Minute,
		cache:        map[string]regionCache{},
		discovered:   map[string]corev1.ResourceList{},
	}
}

func (p *DefaultProvider) List(ctx context.Context, region string) ([]*karpcloudprovider.InstanceType, error) {
	c, err := p.load(ctx, region)
	if err != nil {
		return nil, err
	}
	return lo.Map(c.classes, func(sc rxtspot.ServerClass, _ int) *karpcloudprovider.InstanceType {
		return translate(sc, c.disks[sc.Name], p.discoveredFor(sc.Name))
	}), nil
}

func (p *DefaultProvider) Get(ctx context.Context, region, name string) (*karpcloudprovider.InstanceType, error) {
	c, err := p.load(ctx, region)
	if err != nil {
		return nil, err
	}
	sc, found := lo.Find(c.classes, func(s rxtspot.ServerClass) bool { return s.Name == name })
	if !found {
		return nil, fmt.Errorf("server class %q not found in region %q", name, region)
	}
	return translate(sc, c.disks[sc.Name], p.discoveredFor(sc.Name)), nil
}

func (p *DefaultProvider) UpdateFromNode(instanceType string, capacity corev1.ResourceList) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range discoveredResources {
		reported, ok := capacity[name]
		if !ok || reported.IsZero() {
			continue
		}
		if seen, ok := p.discovered[instanceType][name]; ok && seen.Cmp(reported) <= 0 {
			continue
		}
		if p.discovered[instanceType] == nil {
			p.discovered[instanceType] = corev1.ResourceList{}
		}
		p.discovered[instanceType][name] = reported
	}
}

func (p *DefaultProvider) discoveredFor(instanceType string) corev1.ResourceList {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discovered[instanceType].DeepCopy()
}

func (p *DefaultProvider) MinBidPrice(ctx context.Context, region, name string) (float64, error) {
	c, err := p.load(ctx, region)
	if err != nil {
		return 0, err
	}
	sc, found := lo.Find(c.classes, func(s rxtspot.ServerClass) bool { return s.Name == name })
	if !found {
		return 0, fmt.Errorf("server class %q not found in region %q", name, region)
	}
	return parsePrice(sc.MinBidPricePerHour), nil
}

func (p *DefaultProvider) load(ctx context.Context, region string) (regionCache, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[region]; ok && time.Since(c.fetched) < p.refreshAfter {
		return c, nil
	}
	list, err := p.client.ListServerClasses(ctx, region)
	if err != nil {
		return regionCache{}, fmt.Errorf("listing server classes in %s: %w", region, err)
	}
	disks, err := p.fetchDisks(ctx)
	if err != nil {
		disks = map[string]string{} // non-fatal: translate falls back to the floor
	}
	c := regionCache{classes: list.Items, disks: disks, fetched: time.Now()}
	p.cache[region] = c
	return c, nil
}

// fetchDisks maps ServerClass name to its raw disk string. The SDK decodes
// resources as {cpu, memory} only, dropping disk, so we read the raw endpoint.
func (p *DefaultProvider) fetchDisks(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.client.BaseURL+serverClassesPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// No Authorization header: the auth transport on this client sets one,
	// overwriting whatever is here. Calling the SDK's Authenticate() to build
	// it by hand would write the client's shared token field from whichever
	// goroutine happened to miss the cache. See pkg/auth.

	resp, err := p.client.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("serverclasses disk lookup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Resources struct {
					Disk string `json:"disk"`
				} `json:"resources"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding serverclasses disk lookup: %w", err)
	}
	disks := make(map[string]string, len(payload.Items))
	for _, it := range payload.Items {
		if it.Spec.Resources.Disk != "" {
			disks[it.Metadata.Name] = it.Spec.Resources.Disk
		}
	}
	return disks, nil
}

func translate(sc rxtspot.ServerClass, disk string, discovered corev1.ResourceList) *karpcloudprovider.InstanceType {
	oh := nodeOverhead()
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:              parseQuantity(sc.Resources.CPU),
		corev1.ResourceMemory:           shave(parseQuantity(sc.Resources.Memory), oh.vmMemoryPercent),
		corev1.ResourceEphemeralStorage: shave(ephemeralStorage(disk), defaultVMDiskOverheadPercent),
		corev1.ResourcePods:             *resource.NewQuantity(defaultPodsPerNode, resource.DecimalSI),
	}
	for _, name := range discoveredResources {
		if q, ok := discovered[name]; ok {
			capacity[name] = q
		}
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
				corev1.ResourceCPU:              oh.kubeReservedCPU,
				corev1.ResourceMemory:           oh.kubeReservedMemory,
				corev1.ResourceEphemeralStorage: resource.MustParse(defaultKubeReservedEphemeralStorage),
			},
			EvictionThreshold: corev1.ResourceList{
				corev1.ResourceMemory:           resource.MustParse(defaultMemoryEvictionThreshold),
				corev1.ResourceEphemeralStorage: percentOf(capacity[corev1.ResourceEphemeralStorage], defaultDiskEvictionPercent),
			},
		},
	}
}

var nodeOverhead = sync.OnceValue(loadNodeOverhead)

type overhead struct {
	kubeReservedCPU    resource.Quantity
	kubeReservedMemory resource.Quantity
	vmMemoryPercent    float64
}

func loadNodeOverhead() overhead {
	return overhead{
		kubeReservedCPU:    envQuantity(envKubeReservedCPU, defaultKubeReservedCPU),
		kubeReservedMemory: envQuantity(envKubeReservedMemory, defaultKubeReservedMemory),
		vmMemoryPercent:    envPercent(envVMMemoryOverhead, defaultVMMemoryOverheadPercent),
	}
}

func envQuantity(key, fallback string) resource.Quantity {
	v := os.Getenv(key)
	if v == "" {
		return resource.MustParse(fallback)
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		log.Log.Error(err, "ignoring unparseable override, using default", "env", key, "value", v, "default", fallback)
		return resource.MustParse(fallback)
	}
	return q
}

func envPercent(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	switch {
	case err != nil:
		log.Log.Error(err, "ignoring unparseable override, using default", "env", key, "value", v, "default", fallback)
		return fallback
	case f < 0 || f >= 1:
		log.Log.Info("ignoring out-of-range override, using default", "env", key, "value", v, "default", fallback)
		return fallback
	}
	return f
}

func shave(q resource.Quantity, pct float64) resource.Quantity {
	if q.IsZero() || pct <= 0 {
		return q
	}
	return *resource.NewQuantity(int64(float64(q.Value())*(1-pct)), resource.BinarySI)
}

func percentOf(q resource.Quantity, pct float64) resource.Quantity {
	return *resource.NewQuantity(int64(float64(q.Value())*pct), resource.BinarySI)
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

func ephemeralStorage(disk string) resource.Quantity {
	if q := parseQuantity(disk); !q.IsZero() {
		return q
	}
	return *resource.NewQuantity(defaultEphemeralStorageGi*(1<<30), resource.BinarySI)
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
