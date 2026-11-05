//go:build virtrigaud_dev

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

package devgate

import (
	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Compiled reports whether the development gate is compiled into this build:
// true, because it was built with the virtrigaud_dev tag.
const Compiled = true

// enable returns types as the enabled set: this build has the gate.
func enable(types []infravirtrigaudiov1beta1.ProviderType) (map[infravirtrigaudiov1beta1.ProviderType]struct{}, error) {
	enabled := make(map[infravirtrigaudiov1beta1.ProviderType]struct{}, len(types))
	for _, t := range types {
		enabled[t] = struct{}{}
	}
	return enabled, nil
}
