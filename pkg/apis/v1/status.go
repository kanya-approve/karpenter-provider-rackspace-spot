/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

import "github.com/awslabs/operatorpkg/status"

const (
	ConditionTypeCloudspaceFound        = "CloudspaceFound"
	ConditionTypeServerClassesDiscovered = "ServerClassesDiscovered"
)

func (n *RackspaceSpotNodeClass) GetConditions() []status.Condition {
	return n.Status.Conditions
}

func (n *RackspaceSpotNodeClass) SetConditions(conditions []status.Condition) {
	n.Status.Conditions = conditions
}

func (n *RackspaceSpotNodeClass) StatusConditions() status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeCloudspaceFound,
		ConditionTypeServerClassesDiscovered,
	).For(n)
}
