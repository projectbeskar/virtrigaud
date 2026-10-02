/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseProviderTmpSizeLimit parses a /tmp sizeLimit setting: a positive
// resource quantity, or ProviderTmpSizeLimitNone (returned as nil, meaning no
// limit). Anything else is an error.
func ParseProviderTmpSizeLimit(value string) (*resource.Quantity, error) {
	if value == ProviderTmpSizeLimitNone {
		return nil, nil
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("invalid /tmp size limit %q: want a positive quantity such as 20Gi, or %q: %w",
			value, ProviderTmpSizeLimitNone, err)
	}
	if q.Sign() <= 0 {
		return nil, fmt.Errorf("invalid /tmp size limit %q: must be positive, or %q for no limit", value, ProviderTmpSizeLimitNone)
	}
	return &q, nil
}
