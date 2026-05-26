/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package operator

import (
	"context"
	"fmt"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpoperator "sigs.k8s.io/karpenter/pkg/operator"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/pricing"
)

func init() {
	utilruntime.Must(apiv1.AddToScheme(kscheme.Scheme))
}

// Operator wraps the Karpenter core operator and adds provider-specific clients.
type Operator struct {
	*karpoperator.Operator

	// SpotClient is the authenticated Rackspace Spot API client.
	SpotClient rxtspot.SpotAPI

	InstanceProvider     instance.Provider
	InstanceTypeProvider instancetype.Provider
	PricingProvider      pricing.Provider
}

// NewOperator builds the provider Operator. The Rackspace refresh token is
// read from the SPOT_REFRESH_TOKEN env var (and other SPOT_* env vars per the
// spot-go-sdk Config).
func NewOperator(ctx context.Context, coreOp *karpoperator.Operator) (context.Context, *Operator) {
	logger := log.FromContext(ctx)

	client, err := rxtspot.NewSpotClient(nil)
	if err != nil {
		logger.Error(err, "failed to construct Rackspace Spot client")
		panic(fmt.Errorf("constructing spot client: %w", err))
	}
	if _, err := client.Authenticate(ctx); err != nil {
		logger.Error(err, "failed to authenticate to Rackspace Spot")
		panic(fmt.Errorf("authenticating to Rackspace Spot: %w", err))
	}
	logger.Info("authenticated to Rackspace Spot")

	pricingProvider := pricing.NewProvider()
	return ctx, &Operator{
		Operator:             coreOp,
		SpotClient:           client,
		InstanceProvider:     instance.NewProvider(client, pricingProvider),
		InstanceTypeProvider: instancetype.NewProvider(client),
		PricingProvider:      pricingProvider,
	}
}
