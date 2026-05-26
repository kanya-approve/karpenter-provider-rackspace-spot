/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1

func init() {
	SchemeBuilder.Register(&RackspaceSpotNodeClass{}, &RackspaceSpotNodeClassList{})
}
