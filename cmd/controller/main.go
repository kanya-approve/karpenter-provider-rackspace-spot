/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	opcontroller "github.com/awslabs/operatorpkg/controller"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	karpoperator "sigs.k8s.io/karpenter/pkg/operator"

	rscloudprovider "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/cloudprovider"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/controllers/nodeclass"
	rsoperator "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/operator"
)

func main() {
	ctx, coreOp := karpoperator.NewOperator()
	ctx, op := rsoperator.NewOperator(ctx, coreOp)

	var raw karpcloudprovider.CloudProvider = rscloudprovider.New(op)
	cp := overlay.Decorate(raw, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cp)

	providerControllers := []opcontroller.Controller{
		nodeclass.NewController(op.GetClient(), op.SpotClient, op.InstanceProvider, op.InstanceTypeProvider),
	}

	op.
		WithControllers(ctx, append(
			controllers.NewControllers(
				ctx,
				op.Manager,
				op.Clock,
				op.GetClient(),
				op.EventRecorder,
				cp,
				raw,
				clusterState,
				op.InstanceTypeStore,
			),
			providerControllers...,
		)...).
		Start(ctx)
}
