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

package vsphere

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestParseCreateRequest_StaticNetwork verifies that the static IP fields of the
// first network attachment (StaticIP/Prefix/Gateway/DNS) flow from NetworksJson
// into VMSpec instead of being silently dropped (regression test for #244).
func TestParseCreateRequest_StaticNetwork(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	provider := &Provider{logger: logger}

	tests := []struct {
		name            string
		networksJSON    string
		expectedName    string
		expectedIP      string
		expectedPrefix  int32
		expectedGateway string
		expectedDNS     string
	}{
		{
			name:            "full static configuration",
			networksJSON:    `[{"NetworkName":"VM Network","StaticIP":"192.168.1.50","Prefix":24,"Gateway":"192.168.1.1","DNS":"8.8.8.8,1.1.1.1"}]`,
			expectedName:    "VM Network",
			expectedIP:      "192.168.1.50",
			expectedPrefix:  24,
			expectedGateway: "192.168.1.1",
			expectedDNS:     "8.8.8.8,1.1.1.1",
		},
		{
			name:           "network name only (DHCP)",
			networksJSON:   `[{"NetworkName":"VM Network"}]`,
			expectedName:   "VM Network",
			expectedIP:     "",
			expectedPrefix: 0,
		},
		{
			name:            "first attachment wins for multi-NIC",
			networksJSON:    `[{"NetworkName":"front","StaticIP":"10.0.0.5","Prefix":16,"Gateway":"10.0.0.1"},{"NetworkName":"back","StaticIP":"10.1.0.5"}]`,
			expectedName:    "front",
			expectedIP:      "10.0.0.5",
			expectedPrefix:  16,
			expectedGateway: "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &providerv1.CreateRequest{
				Name:         "test-vm",
				NetworksJson: tt.networksJSON,
			}

			spec, err := provider.parseCreateRequest(req)
			require.NoError(t, err)
			require.NotNil(t, spec)

			assert.Equal(t, tt.expectedName, spec.NetworkName)
			assert.Equal(t, tt.expectedIP, spec.StaticIP)
			assert.Equal(t, tt.expectedPrefix, spec.Prefix)
			assert.Equal(t, tt.expectedGateway, spec.Gateway)
			assert.Equal(t, tt.expectedDNS, spec.DNS)
		})
	}
}

// TestParseCreateRequest_NoNetworks ensures an absent NetworksJson leaves the
// network fields empty without error.
func TestParseCreateRequest_NoNetworks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	provider := &Provider{logger: logger}

	spec, err := provider.parseCreateRequest(&providerv1.CreateRequest{Name: "test-vm"})
	require.NoError(t, err)
	require.NotNil(t, spec)

	assert.Empty(t, spec.NetworkName)
	assert.Empty(t, spec.StaticIP)
	assert.Zero(t, spec.Prefix)
}
