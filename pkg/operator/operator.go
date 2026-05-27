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
	"os"

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

	// CloudspaceName identifies the single Cloudspace this operator manages.
	// Read from SPOT_CLOUDSPACE_NAME at startup; required.
	CloudspaceName string
	// Region is the Cloudspace's region, resolved once at startup via the SDK.
	Region string
	// OrganizationID is the Rackspace org owning the Cloudspace, resolved once.
	OrganizationID string

	InstanceProvider     instance.Provider
	InstanceTypeProvider instancetype.Provider
	PricingProvider      pricing.Provider
}

// NewOperator builds the provider Operator. The Rackspace refresh token is
// read from SPOT_REFRESH_TOKEN; the target Cloudspace from SPOT_CLOUDSPACE_NAME.
// The operator validates both by calling the SDK at startup and panics on
// failure — there's nothing to do if either is wrong.
func NewOperator(ctx context.Context, coreOp *karpoperator.Operator) (context.Context, *Operator) {
	logger := log.FromContext(ctx)

	cloudspaceName := os.Getenv("SPOT_CLOUDSPACE_NAME")
	if cloudspaceName == "" {
		panic("SPOT_CLOUDSPACE_NAME environment variable is required")
	}

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

	orgs, err := client.ListOrganizations(ctx)
	if err != nil {
		panic(fmt.Errorf("listing organizations: %w", err))
	}
	if len(orgs) == 0 {
		panic("authenticated principal has no organizations")
	}
	orgID := orgs[0].ID

	cs, err := client.GetCloudspace(ctx, orgID, cloudspaceName)
	if err != nil {
		panic(fmt.Errorf("cloudspace %q not found in org %q: %w", cloudspaceName, orgID, err))
	}
	if cs.Region == "" {
		panic(fmt.Errorf("cloudspace %q has no region", cloudspaceName))
	}
	logger.Info("resolved cloudspace", "name", cloudspaceName, "region", cs.Region)

	pricingProvider := pricing.NewProvider()
	instanceTypeProvider := instancetype.NewProvider(client)
	return ctx, &Operator{
		Operator:             coreOp,
		SpotClient:           client,
		CloudspaceName:       cloudspaceName,
		Region:               cs.Region,
		OrganizationID:       orgID,
		InstanceProvider:     instance.NewProvider(client, pricingProvider, instanceTypeProvider, cloudspaceName, orgID),
		InstanceTypeProvider: instanceTypeProvider,
		PricingProvider:      pricingProvider,
	}
}
