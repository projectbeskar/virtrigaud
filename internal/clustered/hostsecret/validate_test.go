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

package hostsecret

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestEndpointPatternMatchesAPI pins the provider-side copy of the endpoint
// pattern to the API's HostEndpointPattern (which the CRD schema test in turn
// pins to the generated admission pattern). If they drift, the apiserver and
// the provider disagree about what a valid Host endpoint is.
func TestEndpointPatternMatchesAPI(t *testing.T) {
	assert.Equal(t, infravirtrigaudiov1beta1.HostEndpointPattern, EndpointPattern)
}

// TestValidateEndpoint is the accepted/rejected table for the provider-side
// re-validation of an inventory entry's endpoint. Every rejected value is a
// shape that, before this check, could reach the libvirt connection URI the
// provider forwards to the hypervisor host's shell.
func TestValidateEndpoint(t *testing.T) {
	accept := []string{
		"qemu+ssh://virt@host-a/system",
		"qemu+ssh://host-a/session",
		"qemu+ssh://virt@host-a.example.com:2222/system",
		"qemu+ssh://root@10.0.0.1/system",
		"qemu+ssh://root@[2001:db8::1]:22/system",
		"grpc://agent-a:9443",
		"grpc://[2001:db8::5]:9443",
	}
	for _, ep := range accept {
		assert.NoError(t, ValidateEndpoint(ep), "endpoint %q must be accepted", ep)
	}

	reject := map[string]string{
		"empty":                 "",
		"semicolon command":     "qemu+ssh://virt@victim/system;id",
		"command substitution":  "qemu+ssh://virt@victim/system$(id)",
		"backticks":             "qemu+ssh://virt@victim/system`id`",
		"pipe":                  "qemu+ssh://virt@victim/system|id",
		"newline":               "qemu+ssh://virt@victim/system\nid",
		"trailing newline":      "qemu+ssh://virt@victim/system\n",
		"percent-encoded space": "qemu+ssh://virt@victim/system%20-c%20id",
		"space":                 "qemu+ssh://virt@victim/system id",
		"path traversal":        "qemu+ssh://virt@victim/system/../x",
		"other path":            "qemu+ssh://virt@victim/default",
		"query string":          "qemu+ssh://virt@victim/system?no_verify=1",
		"shell in user":         "qemu+ssh://v$(id)@victim/system",
		"unbracketed ipv6":      "qemu+ssh://2001:db8::1/system",
		"non-ssh transport":     "qemu+tcp://victim/system",
		"grpc without port":     "grpc://agent-a",
		"too long":              "qemu+ssh://virt@" + strings.Repeat("a", 600) + "/system",
	}
	for name, ep := range reject {
		err := ValidateEndpoint(ep)
		require.Error(t, err, "%s: endpoint %q must be rejected", name, ep)
		assert.True(t, errors.Is(err, ErrInvalidEndpoint), "%s: error must wrap ErrInvalidEndpoint", name)
		if ep != "" {
			assert.NotContains(t, err.Error(), ep, "%s: the error must never echo the raw endpoint", name)
		}
	}
}

// TestMarshal_RejectsDuplicateHostIDs proves an ambiguous inventory (two hosts,
// one id) is refused outright rather than rendered with whichever entry the
// sort happens to place first.
func TestMarshal_RejectsDuplicateHostIDs(t *testing.T) {
	inv := Inventory{
		SchemaVersion: SchemaVersion,
		Hosts: []Host{
			{ID: "host-a", Endpoint: "qemu+ssh://virt@a1/system"},
			{ID: "host-b", Endpoint: "qemu+ssh://virt@b/system"},
			{ID: "host-a", Endpoint: "qemu+ssh://virt@a2/system"},
		},
	}
	_, err := Marshal(inv)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateHostID))
	assert.Contains(t, err.Error(), "host-a")
}
