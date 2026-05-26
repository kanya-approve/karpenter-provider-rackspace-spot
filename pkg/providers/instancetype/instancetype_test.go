/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package instancetype

import (
	"testing"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestTranslate_CPUOnly(t *testing.T) {
	sc := rxtspot.ServerClass{
		Name:                      "gp.vs1.small-dfw",
		Region:                    "us-central-dfw-1",
		Availability:              "available",
		OnDemandPricePerHour:      "0.10",
		CurrentMarketPricePerHour: "0.04",
		Resources:                 rxtspot.Resource{CPU: "2", Memory: "8Gi"},
	}
	it := translate(sc)

	if it.Name != sc.Name {
		t.Errorf("Name = %q, want %q", it.Name, sc.Name)
	}
	if got := it.Capacity[corev1.ResourceCPU]; got.String() != "2" {
		t.Errorf("CPU capacity = %q, want 2", got.String())
	}
	if got := it.Capacity[corev1.ResourceMemory]; got.String() != "8Gi" {
		t.Errorf("Memory capacity = %q, want 8Gi", got.String())
	}
	if _, hasGPU := it.Capacity[resourceNvidiaGPU]; hasGPU {
		t.Errorf("expected no GPU capacity for CPU-only ServerClass")
	}
	if pods := it.Capacity[corev1.ResourcePods]; pods.Value() != defaultPodsPerNode {
		t.Errorf("Pods capacity = %d, want %d", pods.Value(), defaultPodsPerNode)
	}
	if len(it.Offerings) != 2 {
		t.Fatalf("expected 2 offerings (spot + on-demand), got %d", len(it.Offerings))
	}
}

func TestTranslate_GPUOnly(t *testing.T) {
	sc := rxtspot.ServerClass{
		Name:                      "gpu.h100-sjc",
		Region:                    "us-west-sjc-1",
		Availability:              "available",
		OnDemandPricePerHour:      "3.50",
		CurrentMarketPricePerHour: "1.10",
		Resources:                 rxtspot.Resource{CPU: "16", Memory: "128Gi", GPU: "1"},
	}
	it := translate(sc)
	if got, ok := it.Capacity[resourceNvidiaGPU]; !ok || got.Value() != 1 {
		t.Errorf("nvidia.com/gpu = %v (ok=%v), want 1", got, ok)
	}
}

func TestTranslate_UnavailableMarksOfferings(t *testing.T) {
	sc := rxtspot.ServerClass{
		Name:                      "gp.vs1.tiny",
		Region:                    "us-central-dfw-1",
		Availability:              "unavailable",
		OnDemandPricePerHour:      "0.02",
		CurrentMarketPricePerHour: "0.01",
		Resources:                 rxtspot.Resource{CPU: "1", Memory: "2Gi"},
	}
	it := translate(sc)
	for _, of := range it.Offerings {
		if of.Available {
			t.Errorf("expected unavailable offerings, got Available=true on %v", of.Requirements)
		}
	}
}

func TestTranslate_ZeroPriceDropsOffering(t *testing.T) {
	sc := rxtspot.ServerClass{
		Name:                 "gp.vs1.no-spot",
		Region:               "us-central-dfw-1",
		Availability:         "available",
		OnDemandPricePerHour: "0.05",
		// No CurrentMarketPricePerHour: spot offering should be omitted.
		Resources: rxtspot.Resource{CPU: "1", Memory: "2Gi"},
	}
	it := translate(sc)
	if len(it.Offerings) != 1 {
		t.Fatalf("expected only on-demand offering, got %d", len(it.Offerings))
	}
	if !it.Offerings[0].Requirements.Has(karpv1.CapacityTypeLabelKey) {
		t.Fatal("offering missing capacity-type requirement")
	}
	got := it.Offerings[0].Requirements.Get(karpv1.CapacityTypeLabelKey).Any()
	if got != karpv1.CapacityTypeOnDemand {
		t.Errorf("expected on-demand offering, got %q", got)
	}
}
