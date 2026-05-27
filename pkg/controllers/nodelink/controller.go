/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package nodelink

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kanya-approve/karpenter-provider-rackspace-spot/pkg/providers/instance"
)

// Controller rewrites NodeClaim.status.providerID to match the joining
// Node.spec.providerID (CCM-set openstack:///<vm-uuid>). We can't return
// the final providerID from Create() because Rackspace's auction decouples
// bid acceptance from server assignment — VM UUID isn't known yet.
type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{kubeClient: kubeClient}
}

func (c *Controller) Register(_ context.Context, mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodelink").
		For(&corev1.Node{}, builder.WithPredicates(managedNode())).
		Complete(c)
}

func managedNode() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[instance.KarpenterManagedLabel] == "true"
	})
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)

	var node corev1.Node
	if err := c.kubeClient.Get(ctx, req.NamespacedName, &node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	nodeClaimName := node.Labels[instance.NodeClaimNameLabel]
	if nodeClaimName == "" {
		return reconcile.Result{}, nil
	}
	if node.Spec.ProviderID == "" {
		return reconcile.Result{}, nil // CCM hasn't set it yet; we'll reconcile again on update
	}

	var nc karpv1.NodeClaim
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaimName}, &nc); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting NodeClaim %q: %w", nodeClaimName, err)
	}

	if nc.Status.ProviderID == node.Spec.ProviderID {
		return reconcile.Result{}, nil
	}

	original := nc.DeepCopy()
	nc.Status.ProviderID = node.Spec.ProviderID
	if err := c.kubeClient.Status().Patch(ctx, &nc, client.MergeFrom(original)); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("patching NodeClaim %q providerID: %w", nodeClaimName, err)
	}
	logger.Info("rewrote NodeClaim providerID", "nodeClaim", nodeClaimName, "providerID", node.Spec.ProviderID)
	return reconcile.Result{}, nil
}
