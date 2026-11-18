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
	"strings"

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
//
// The NodeClaim is found by the pool's custom labels when present, but
// Rackspace's Cluster API only copies those onto the Node minutes after it
// joins. The cluster.x-k8s.io/machine annotation and providerID are there
// within seconds, and the machine name starts with the pool name, which is
// the NodeClaim UID. Linking on that instead keeps a slow label sync from
// running the claim past Karpenter's 15m registration timeout.
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
		return obj.GetLabels()[instance.KarpenterManagedLabel] == "true" ||
			obj.GetAnnotations()[instance.MachineAnnotation] != ""
	})
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)

	var node corev1.Node
	if err := c.kubeClient.Get(ctx, req.NamespacedName, &node); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if node.Spec.ProviderID == "" {
		return reconcile.Result{}, nil // CCM hasn't set it yet; we'll reconcile again on update
	}

	nc, err := c.nodeClaimFor(ctx, &node)
	if err != nil {
		return reconcile.Result{}, err
	}
	if nc == nil {
		return reconcile.Result{}, nil
	}
	nodeClaimName := nc.Name

	if nc.Status.ProviderID == node.Spec.ProviderID {
		return reconcile.Result{}, nil
	}

	original := nc.DeepCopy()
	nc.Status.ProviderID = node.Spec.ProviderID
	if err := c.kubeClient.Status().Patch(ctx, nc, client.MergeFrom(original)); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, fmt.Errorf("patching NodeClaim %q providerID: %w", nodeClaimName, err)
	}
	logger.Info("rewrote NodeClaim providerID", "nodeClaim", nodeClaimName, "providerID", node.Spec.ProviderID)
	return reconcile.Result{}, nil
}

// nodeClaimFor returns the NodeClaim whose pool the Node belongs to, or nil if
// it isn't one of ours (yet).
func (c *Controller) nodeClaimFor(ctx context.Context, node *corev1.Node) (*karpv1.NodeClaim, error) {
	if name := node.Labels[instance.NodeClaimNameLabel]; name != "" {
		var nc karpv1.NodeClaim
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: name}, &nc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("getting NodeClaim %q: %w", name, err)
		}
		return &nc, nil
	}

	machine := node.Annotations[instance.MachineAnnotation]
	if machine == "" {
		return nil, nil
	}
	var ncs karpv1.NodeClaimList
	if err := c.kubeClient.List(ctx, &ncs); err != nil {
		return nil, fmt.Errorf("listing NodeClaims: %w", err)
	}
	for i := range ncs.Items {
		if uid := ncs.Items[i].UID; uid != "" && strings.HasPrefix(machine, string(uid)+"-") {
			return &ncs.Items[i], nil
		}
	}
	return nil, nil // e.g. the Cloudspace's own base nodepool
}
