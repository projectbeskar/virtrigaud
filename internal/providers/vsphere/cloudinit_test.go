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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/vim25/types"
)

func TestAddCloudInitToConfigSpec(t *testing.T) {
	tests := []struct {
		name              string
		cloudInitData     string
		cloudInitMetaData string
		expectedUserData  string
		expectedMetadata  string
		expectedEncoding  string
	}{
		{
			name: "with custom metadata in YAML",
			cloudInitData: `#cloud-config
users:
  - name: admin`,
			cloudInitMetaData: `instance-id: custom-vm-001
local-hostname: app-server
region: us-west-2`,
			expectedUserData: `#cloud-config
users:
  - name: admin`,
			expectedMetadata: `instance-id: custom-vm-001
local-hostname: app-server
region: us-west-2`,
			expectedEncoding: "yaml",
		},
		{
			name: "without custom metadata (default)",
			cloudInitData: `#cloud-config
packages:
  - nginx`,
			cloudInitMetaData: "",
			expectedUserData: `#cloud-config
packages:
  - nginx`,
			expectedMetadata: `{"instance-id": "test-vm"}`,
			expectedEncoding: "json",
		},
		{
			name: "with network configuration in metadata",
			cloudInitData: `#cloud-config
runcmd:
  - echo "Hello"`,
			cloudInitMetaData: `instance-id: network-vm
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true`,
			expectedUserData: `#cloud-config
runcmd:
  - echo "Hello"`,
			expectedMetadata: `instance-id: network-vm
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true`,
			expectedEncoding: "yaml",
		},
		{
			name: "with public-keys in metadata",
			cloudInitData: `#cloud-config
packages: []`,
			cloudInitMetaData: `instance-id: ssh-vm
public-keys:
  - ssh-rsa AAAAB3... key1@example.com
  - ssh-rsa AAAAB3... key2@example.com`,
			expectedUserData: `#cloud-config
packages: []`,
			expectedMetadata: `instance-id: ssh-vm
public-keys:
  - ssh-rsa AAAAB3... key1@example.com
  - ssh-rsa AAAAB3... key2@example.com`,
			expectedEncoding: "yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create provider instance
			provider := &Provider{
				// No logger needed for this test
			}

			// Create config spec
			configSpec := &types.VirtualMachineConfigSpec{
				Name: "test-vm",
			}

			// Call the function
			err := provider.addCloudInitToConfigSpec(configSpec, tt.cloudInitData, tt.cloudInitMetaData)
			require.NoError(t, err)

			// Verify ExtraConfig was set
			require.NotNil(t, configSpec.ExtraConfig)
			require.Len(t, configSpec.ExtraConfig, 4, "Should have 4 guestinfo properties")

			// Convert to map for easier testing
			extraConfigMap := make(map[string]string)
			for _, option := range configSpec.ExtraConfig {
				optionValue, ok := option.(*types.OptionValue)
				require.True(t, ok, "option should be *types.OptionValue")
				value, ok := optionValue.Value.(string)
				require.True(t, ok, "value should be string")
				extraConfigMap[optionValue.Key] = value
			}

			// Verify guestinfo.userdata
			assert.Equal(t, tt.expectedUserData, extraConfigMap["guestinfo.userdata"], "userdata should match")
			assert.Equal(t, "yaml", extraConfigMap["guestinfo.userdata.encoding"], "userdata encoding should be yaml")

			// Verify guestinfo.metadata
			assert.Equal(t, tt.expectedMetadata, extraConfigMap["guestinfo.metadata"], "metadata should match")
			assert.Equal(t, tt.expectedEncoding, extraConfigMap["guestinfo.metadata.encoding"], "metadata encoding should match")
		})
	}
}

func TestAddCloudInitToConfigSpec_WithExistingExtraConfig(t *testing.T) {
	provider := &Provider{}

	configSpec := &types.VirtualMachineConfigSpec{
		Name: "test-vm",
		ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{
				Key:   "some.existing.key",
				Value: "existing-value",
			},
		},
	}

	cloudInitData := `#cloud-config
packages:
  - docker`
	cloudInitMetaData := `instance-id: test-vm-001
region: us-east-1`

	err := provider.addCloudInitToConfigSpec(configSpec, cloudInitData, cloudInitMetaData)
	require.NoError(t, err)

	// Should have 5 items total (1 existing + 4 new)
	require.Len(t, configSpec.ExtraConfig, 5)

	// Convert to map
	extraConfigMap := make(map[string]string)
	for _, option := range configSpec.ExtraConfig {
		optionValue, ok := option.(*types.OptionValue)
		require.True(t, ok, "option should be *types.OptionValue")
		value, ok := optionValue.Value.(string)
		require.True(t, ok, "value should be string")
		extraConfigMap[optionValue.Key] = value
	}

	// Verify existing config is preserved
	assert.Equal(t, "existing-value", extraConfigMap["some.existing.key"])

	// Verify new cloud-init config is added
	assert.Equal(t, cloudInitData, extraConfigMap["guestinfo.userdata"])
	assert.Equal(t, "yaml", extraConfigMap["guestinfo.userdata.encoding"])
	assert.Equal(t, cloudInitMetaData, extraConfigMap["guestinfo.metadata"])
	assert.Equal(t, "yaml", extraConfigMap["guestinfo.metadata.encoding"])
}

func TestAddCloudInitToConfigSpec_EmptyMetaDataUsesDefault(t *testing.T) {
	provider := &Provider{}

	configSpec := &types.VirtualMachineConfigSpec{
		Name: "my-test-vm",
	}

	cloudInitData := `#cloud-config
packages:
  - curl`

	err := provider.addCloudInitToConfigSpec(configSpec, cloudInitData, "")
	require.NoError(t, err)

	// Convert to map
	extraConfigMap := make(map[string]string)
	for _, option := range configSpec.ExtraConfig {
		optionValue, ok := option.(*types.OptionValue)
		require.True(t, ok, "option should be *types.OptionValue")
		value, ok := optionValue.Value.(string)
		require.True(t, ok, "value should be string")
		extraConfigMap[optionValue.Key] = value
	}

	// Verify default metadata uses VM name in JSON format
	assert.Equal(t, `{"instance-id": "my-test-vm"}`, extraConfigMap["guestinfo.metadata"])
	assert.Equal(t, "json", extraConfigMap["guestinfo.metadata.encoding"])
}

func TestAddCloudInitToConfigSpec_ComplexMetaData(t *testing.T) {
	provider := &Provider{}

	configSpec := &types.VirtualMachineConfigSpec{
		Name: "complex-vm",
	}

	cloudInitData := `#cloud-config
users:
  - name: ubuntu`

	cloudInitMetaData := `instance-id: complex-vm-001
local-hostname: complex-server.example.com
network:
  version: 2
  ethernets:
    eth0:
      addresses:
        - 192.168.1.100/24
      gateway4: 192.168.1.1
      nameservers:
        addresses:
          - 8.8.8.8
          - 8.8.4.4
custom:
  region: us-west-2
  availability-zone: us-west-2a
  environment: production
  project: web-app
  tags:
    - production
    - web
    - critical`

	err := provider.addCloudInitToConfigSpec(configSpec, cloudInitData, cloudInitMetaData)
	require.NoError(t, err)

	// Convert to map
	extraConfigMap := make(map[string]string)
	for _, option := range configSpec.ExtraConfig {
		optionValue, ok := option.(*types.OptionValue)
		require.True(t, ok, "option should be *types.OptionValue")
		value, ok := optionValue.Value.(string)
		require.True(t, ok, "value should be string")
		extraConfigMap[optionValue.Key] = value
	}

	// Verify complex metadata is preserved exactly
	metadata := extraConfigMap["guestinfo.metadata"]
	assert.Contains(t, metadata, "instance-id: complex-vm-001")
	assert.Contains(t, metadata, "local-hostname: complex-server.example.com")
	assert.Contains(t, metadata, "network:")
	assert.Contains(t, metadata, "version: 2")
	assert.Contains(t, metadata, "192.168.1.100/24")
	assert.Contains(t, metadata, "gateway4: 192.168.1.1")
	assert.Contains(t, metadata, "8.8.8.8")
	assert.Contains(t, metadata, "region: us-west-2")
	assert.Contains(t, metadata, "availability-zone: us-west-2a")
	assert.Contains(t, metadata, "environment: production")
	assert.Contains(t, metadata, "- production")
	assert.Contains(t, metadata, "- web")
	assert.Contains(t, metadata, "- critical")
	assert.Equal(t, "yaml", extraConfigMap["guestinfo.metadata.encoding"])
}

func TestVMSpec_CloudInitMetaData(t *testing.T) {
	tests := []struct {
		name                    string
		cloudInit               string
		cloudInitMetaData       string
		expectCloudInit         string
		expectCloudInitMetaData string
	}{
		{
			name:                    "both fields populated",
			cloudInit:               "#cloud-config\npackages: [nginx]",
			cloudInitMetaData:       "instance-id: vm-001\nregion: us-west-2",
			expectCloudInit:         "#cloud-config\npackages: [nginx]",
			expectCloudInitMetaData: "instance-id: vm-001\nregion: us-west-2",
		},
		{
			name:                    "only cloudInit",
			cloudInit:               "#cloud-config\nusers: []",
			cloudInitMetaData:       "",
			expectCloudInit:         "#cloud-config\nusers: []",
			expectCloudInitMetaData: "",
		},
		{
			name:                    "empty fields",
			cloudInit:               "",
			cloudInitMetaData:       "",
			expectCloudInit:         "",
			expectCloudInitMetaData: "",
		},
		{
			name:                    "metadata without userdata",
			cloudInit:               "",
			cloudInitMetaData:       "instance-id: metadata-only\nenvironment: test",
			expectCloudInit:         "",
			expectCloudInitMetaData: "instance-id: metadata-only\nenvironment: test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := VMSpec{
				Name:              "test-vm",
				CloudInit:         tt.cloudInit,
				CloudInitMetaData: tt.cloudInitMetaData,
			}

			assert.Equal(t, tt.expectCloudInit, spec.CloudInit)
			assert.Equal(t, tt.expectCloudInitMetaData, spec.CloudInitMetaData)
		})
	}
}
