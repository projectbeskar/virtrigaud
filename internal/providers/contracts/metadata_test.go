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

package contracts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCreateRequest_WithMetaData(t *testing.T) {
	tests := []struct {
		name               string
		req                CreateRequest
		expectMetaDataNil  bool
		expectMetaDataYAML string
	}{
		{
			name: "with metadata",
			req: CreateRequest{
				Name: "test-vm",
				MetaData: &MetaData{
					MetaDataYAML: "instance-id: test-001\nregion: us-west-2",
				},
			},
			expectMetaDataNil:  false,
			expectMetaDataYAML: "instance-id: test-001\nregion: us-west-2",
		},
		{
			name: "without metadata",
			req: CreateRequest{
				Name:     "test-vm",
				MetaData: nil,
			},
			expectMetaDataNil:  true,
			expectMetaDataYAML: "",
		},
		{
			name: "with empty metadata",
			req: CreateRequest{
				Name: "test-vm",
				MetaData: &MetaData{
					MetaDataYAML: "",
				},
			},
			expectMetaDataNil:  false,
			expectMetaDataYAML: "",
		},
		{
			name: "with complex metadata",
			req: CreateRequest{
				Name: "test-vm",
				MetaData: &MetaData{
					MetaDataYAML: `instance-id: complex-vm
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
custom:
  region: us-east-1
  tags:
    - production`,
				},
			},
			expectMetaDataNil: false,
			expectMetaDataYAML: `instance-id: complex-vm
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
custom:
  region: us-east-1
  tags:
    - production`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.expectMetaDataNil {
				assert.Nil(t, tt.req.MetaData)
			} else {
				assert.NotNil(t, tt.req.MetaData)
				assert.Equal(t, tt.expectMetaDataYAML, tt.req.MetaData.MetaDataYAML)
			}
		})
	}
}

func TestCreateRequest_WithUserDataAndMetaData(t *testing.T) {
	req := CreateRequest{
		Name: "full-vm",
		UserData: &UserData{
			CloudInitData: "#cloud-config\npackages:\n  - nginx",
			Type:          "cloud-init",
		},
		MetaData: &MetaData{
			MetaDataYAML: "instance-id: full-vm-001\nlocal-hostname: full-server",
		},
	}

	assert.NotNil(t, req.UserData)
	assert.Equal(t, "#cloud-config\npackages:\n  - nginx", req.UserData.CloudInitData)
	assert.Equal(t, "cloud-init", req.UserData.Type)

	assert.NotNil(t, req.MetaData)
	assert.Equal(t, "instance-id: full-vm-001\nlocal-hostname: full-server", req.MetaData.MetaDataYAML)
}

func TestMetaData_EmptyVsNil(t *testing.T) {
	t.Run("nil metadata", func(t *testing.T) {
		var md *MetaData
		assert.Nil(t, md)
	})

	t.Run("empty metadata", func(t *testing.T) {
		md := &MetaData{
			MetaDataYAML: "",
		}
		assert.NotNil(t, md)
		assert.Equal(t, "", md.MetaDataYAML)
	})

	t.Run("metadata with value", func(t *testing.T) {
		md := &MetaData{
			MetaDataYAML: "instance-id: test",
		}
		assert.NotNil(t, md)
		assert.Equal(t, "instance-id: test", md.MetaDataYAML)
	})
}

func TestCreateRequest_MetaDataPreservesFormatting(t *testing.T) {
	yamlWithFormatting := `instance-id: formatted-vm
local-hostname: formatted-server.example.com

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
  environment: production
  tags:
    - web
    - production`

	req := CreateRequest{
		Name: "formatted-vm",
		MetaData: &MetaData{
			MetaDataYAML: yamlWithFormatting,
		},
	}

	assert.NotNil(t, req.MetaData)
	assert.Equal(t, yamlWithFormatting, req.MetaData.MetaDataYAML)

	// Verify specific formatting is preserved
	assert.Contains(t, req.MetaData.MetaDataYAML, "\n\n")      // blank lines
	assert.Contains(t, req.MetaData.MetaDataYAML, "    eth0:") // indentation
	assert.Contains(t, req.MetaData.MetaDataYAML, "  - web")   // list formatting
}

func TestCreateRequest_MetaDataWithSpecialCharacters(t *testing.T) {
	tests := []struct {
		name         string
		metaDataYAML string
	}{
		{
			name:         "with colons in values",
			metaDataYAML: "instance-id: vm:with:colons\nurl: https://example.com:8080",
		},
		{
			name: "with quotes",
			metaDataYAML: `instance-id: "quoted-vm"
message: 'single quoted'`,
		},
		{
			name: "with multiline string",
			metaDataYAML: `instance-id: multiline-vm
description: |
  This is a multiline
  description with
  multiple lines`,
		},
		{
			name: "with special yaml characters",
			metaDataYAML: `instance-id: special-vm
special: "value with @#$%^&*() characters"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := CreateRequest{
				Name: "special-vm",
				MetaData: &MetaData{
					MetaDataYAML: tt.metaDataYAML,
				},
			}

			assert.NotNil(t, req.MetaData)
			assert.Equal(t, tt.metaDataYAML, req.MetaData.MetaDataYAML)
		})
	}
}
