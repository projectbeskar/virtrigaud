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

package clustered

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the host-encoded task reference format of a clustered
// provider (ADR-0007 Addendum A, A1, slice 3) and the refusal of any reference
// that is malformed. The routing of TaskStatus to the encoded host is pinned
// by each provider's own tests.

func TestHostTaskRef_RoundTrip(t *testing.T) {
	ref := EncodeHostTaskRef("host-a.rack-1", "job/42:x")
	assert.Equal(t, "host-task/v1/host-a.rack-1/job/42:x", ref)
	host, inner, err := ParseHostTaskRef(ref)
	require.NoError(t, err)
	assert.Equal(t, HostID("host-a.rack-1"), host)
	assert.Equal(t, "job/42:x", inner, "the host-local reference is kept verbatim, '/' included")

	assert.Empty(t, EncodeHostTaskRef("host-a", ""), "a synchronous call has no task, and no reference")
}

func TestHostTaskRef_ParseRefusesMalformed(t *testing.T) {
	for name, ref := range map[string]string{
		"empty":                    "",
		"not host-encoded":         "task-123",
		"other version":            "host-task/v2/host-a/t",
		"no task":                  "host-task/v1/host-a",
		"empty task":               "host-task/v1/host-a/",
		"empty host":               "host-task/v1//t",
		"uppercase host":           "host-task/v1/Host-A/t",
		"host with a port":         "host-task/v1/host-a:22/t",
		"host is a path":           "host-task/v1/../t",
		"host is an endpoint":      "host-task/v1/qemu+ssh:/t",
		"host with leading hyphen": "host-task/v1/-host/t",
		"too long":                 "host-task/v1/host-a/" + strings.Repeat("x", HostTaskRefMaxLen),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseHostTaskRef(ref)
			assert.Error(t, err)
		})
	}
}
