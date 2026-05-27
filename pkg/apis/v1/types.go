/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import (
	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RackspaceSpotNodeClass selects which Rackspace ServerClasses (instance
// types) are eligible for provisioning and customizes labels/taints/
// annotations on the resulting pools. The Cloudspace itself is configured
// once at operator startup (SPOT_CLOUDSPACE_NAME env var) — not on the
// NodeClass.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=rackspacespotnodeclasses,scope=Cluster,categories=karpenter,shortName={rsnc,rsncs}
// +kubebuilder:subresource:status
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
	// ServerClassSelector narrows which ServerClasses are considered when
	// scheduling. If unset, all ServerClasses in the Cloudspace's region are
	// eligible.
	//
	// +optional
	ServerClassSelector *metav1.LabelSelector `json:"serverClassSelector,omitempty"`

	// BidPercentile picks which percentile of Rackspace's published 30-day
	// price distribution the controller bids at for spot pools. Lower
	// percentiles mean cheaper bids and more frequent preemptions during
	// volatile periods; higher percentiles hold through more price ticks.
	// The actual bid is max(current_market, percentile_value) * 1.05.
	//
	// +kubebuilder:validation:Enum=P20;P50;P80
	// +kubebuilder:default=P80
	// +optional
	BidPercentile string `json:"bidPercentile,omitempty"`

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
	// Standard types include "Ready" and "ServerClassesDiscovered".
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []status.Condition `json:"conditions,omitempty"`

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
