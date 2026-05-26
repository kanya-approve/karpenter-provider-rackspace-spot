/*
Copyright 2026 The karpenter-provider-rackspace-spot Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RackspaceSpotNodeClass binds a Karpenter NodePool to a specific Rackspace
// Spot Cloudspace and selects which ServerClasses (instance types) are
// eligible for provisioning.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=rackspacespotnodeclasses,scope=Cluster,categories=karpenter,shortName={rsnc,rsncs}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cloudspace",type="string",JSONPath=".spec.cloudspaceName"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type RackspaceSpotNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RackspaceSpotNodeClassSpec   `json:"spec,omitempty"`
	Status RackspaceSpotNodeClassStatus `json:"status,omitempty"`
}

// RackspaceSpotNodeClassList contains a list of RackspaceSpotNodeClass.
//
// +kubebuilder:object:root=true
type RackspaceSpotNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RackspaceSpotNodeClass `json:"items"`
}

// RackspaceSpotNodeClassSpec is the user-facing configuration for a
// RackspaceSpotNodeClass.
type RackspaceSpotNodeClassSpec struct {
	// CloudspaceName is the Rackspace Spot Cloudspace into which pools are
	// provisioned. The Cloudspace must exist before the NodeClass is
	// referenced by a NodePool.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CloudspaceName string `json:"cloudspaceName"`

	// BidPrice is the per-hour USD bid used for SpotNodePools. Required when
	// a NodePool referencing this NodeClass requests capacity-type=spot.
	// Ignored for on-demand provisioning.
	//
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,3})?$`
	// +optional
	BidPrice string `json:"bidPrice,omitempty"`

	// ServerClassSelector narrows which ServerClasses are considered when
	// scheduling. If unset, all ServerClasses in the Cloudspace's region are
	// eligible.
	//
	// +optional
	ServerClassSelector *metav1.LabelSelector `json:"serverClassSelector,omitempty"`

	// Labels are propagated to the underlying Rackspace pool's CustomLabels
	// and thus to the resulting Kubernetes Node labels.
	//
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are propagated to the underlying Rackspace pool's
	// CustomAnnotations.
	//
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Taints are propagated to the underlying Rackspace pool's CustomTaints
	// and thus to the resulting Kubernetes Node taints.
	//
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
}

// RackspaceSpotNodeClassStatus reflects observed state.
type RackspaceSpotNodeClassStatus struct {
	// Conditions represent the latest observations of the NodeClass state.
	// Standard types include "Ready", "CloudspaceFound", "ServerClassesDiscovered".
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ServerClasses lists the names of ServerClasses currently eligible for
	// scheduling (post-selector filtering). Refreshed periodically.
	//
	// +optional
	ServerClasses []string `json:"serverClasses,omitempty"`

	// Region is the Cloudspace's region, cached for quick lookups.
	//
	// +optional
	Region string `json:"region,omitempty"`
}
