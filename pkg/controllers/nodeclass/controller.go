/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package nodeclass

import (
	"context"
	"fmt"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	"github.com/samber/lo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/apis/v1"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instancetype"
)

// requeueAfter is the steady-state polling interval used to refresh
// ServerClass discovery and detect Cloudspace-side changes.
const requeueAfter = 5 * time.Minute

// Controller reconciles RackspaceSpotNodeClass: validates the referenced
// Cloudspace exists, caches its region, and refreshes the eligible
// ServerClass list into Status.
//
// +kubebuilder:rbac:groups=karpenter.rackspace.com,resources=rackspacespotnodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=karpenter.rackspace.com,resources=rackspacespotnodeclasses/status,verbs=patch;update
type Controller struct {
	kubeClient   client.Client
	spotAPI      rxtspot.SpotAPI
	instances    instance.Provider
	instanceType instancetype.Provider
}

func NewController(kubeClient client.Client, spotAPI rxtspot.SpotAPI, instances instance.Provider, instanceType instancetype.Provider) *Controller {
	return &Controller{
		kubeClient:   kubeClient,
		spotAPI:      spotAPI,
		instances:    instances,
		instanceType: instanceType,
	}
}

func (c *Controller) Register(_ context.Context, mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeclass.status").
		For(&apiv1.RackspaceSpotNodeClass{}).
		Complete(c)
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("nodeclass", req.Name)

	var nc apiv1.RackspaceSpotNodeClass
	if err := c.kubeClient.Get(ctx, req.NamespacedName, &nc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	original := nc.DeepCopy()

	if err := c.reconcileCloudspace(ctx, &nc); err != nil {
		logger.Error(err, "cloudspace lookup failed")
	}
	if nc.Status.Region != "" {
		if err := c.reconcileServerClasses(ctx, &nc); err != nil {
			logger.Error(err, "server class discovery failed")
		}
	}

	if err := c.patchStatus(ctx, original, &nc); err != nil {
		return reconcile.Result{}, fmt.Errorf("patching NodeClass status: %w", err)
	}
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

func (c *Controller) reconcileCloudspace(ctx context.Context, nc *apiv1.RackspaceSpotNodeClass) error {
	if nc.Spec.CloudspaceName == "" {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeCloudspaceFound, "MissingCloudspaceName", "spec.cloudspaceName is empty")
		return nil
	}
	org, err := c.instances.OrganizationID(ctx)
	if err != nil {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeCloudspaceFound, "OrganizationLookupFailed", err.Error())
		return err
	}
	cs, err := c.spotAPI.GetCloudspace(ctx, org, nc.Spec.CloudspaceName)
	if err != nil {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeCloudspaceFound, "GetCloudspaceFailed", err.Error())
		return err
	}
	nc.Status.Region = cs.Region
	nc.StatusConditions().SetTrue(apiv1.ConditionTypeCloudspaceFound)
	return nil
}

func (c *Controller) reconcileServerClasses(ctx context.Context, nc *apiv1.RackspaceSpotNodeClass) error {
	its, err := c.instanceType.List(ctx, nc.Status.Region)
	if err != nil {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeServerClassesDiscovered, "ListServerClassesFailed", err.Error())
		return err
	}
	nc.Status.ServerClasses = lo.Map(its, func(it *karpcloudprovider.InstanceType, _ int) string { return it.Name })
	nc.StatusConditions().SetTrue(apiv1.ConditionTypeServerClassesDiscovered)
	return nil
}

func (c *Controller) patchStatus(ctx context.Context, original, updated *apiv1.RackspaceSpotNodeClass) error {
	if err := c.kubeClient.Status().Patch(ctx, updated, client.MergeFrom(original)); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}
