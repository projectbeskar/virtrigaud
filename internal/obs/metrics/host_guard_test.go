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

package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRecordHostGuardRefusal pins the host-guard refusal counter and its
// low-cardinality labels (provider type and reason only).
func TestRecordHostGuardRefusal(t *testing.T) {
	labels := map[string]string{"provider_type": "libvirt", "reason": HostGuardReasonTargetSymlink}
	before := counterValue(t, "virtrigaud_provider_host_guard_refusals_total", labels)
	RecordHostGuardRefusal("libvirt", HostGuardReasonTargetSymlink)
	assert.Equal(t, before+1, counterValue(t, "virtrigaud_provider_host_guard_refusals_total", labels))
}
