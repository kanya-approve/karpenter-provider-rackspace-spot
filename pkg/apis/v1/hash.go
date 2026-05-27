/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Hash is a stable digest of the drift-relevant subset of the NodeClass
// spec. The CloudProvider stamps this onto each NodeClaim's annotations
// at Create time; IsDrifted recomputes from the current spec and compares.
// Mismatch → Karpenter rolls the node.
//
// Fields included (changes here trigger drift):
//   - CloudspaceName: a pool in the wrong Cloudspace is fundamentally drifted.
//   - ServerClassSelector: narrows the eligible ServerClasses; a tightened
//     selector should drop nodes that no longer qualify.
//   - Labels / Annotations / Taints: propagated to the pool's CustomLabels /
//     CustomAnnotations / CustomTaints at Create; live changes should roll.
//
// Fields excluded:
//   - BidPercentile — only affects future bids, not the existing pool's
//     accepted bid. Changing it doesn't make existing nodes wrong.
func (n *RackspaceSpotNodeClass) Hash() string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(struct {
		CloudspaceName      string                `json:"cloudspaceName"`
		ServerClassSelector *metav1.LabelSelector `json:"serverClassSelector"`
		Labels              map[string]string     `json:"labels"`
		Annotations         map[string]string     `json:"annotations"`
		Taints              []corev1.Taint        `json:"taints"`
	}{
		CloudspaceName:      n.Spec.CloudspaceName,
		ServerClassSelector: n.Spec.ServerClassSelector,
		Labels:              n.Spec.Labels,
		Annotations:         n.Spec.Annotations,
		Taints:              n.Spec.Taints,
	})
	return hex.EncodeToString(h.Sum(nil))
}
