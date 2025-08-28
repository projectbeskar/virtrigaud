/*
Copyright 2025.

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

package proxmox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxmoxProvider_Validate(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()
	resp, err := provider.Validate(ctx, &providerv1.ValidateRequest{})

	require.NoError(t, err)
	assert.True(t, resp.Ok)
	assert.Contains(t, resp.Message, "Proxmox VE provider is ready")
}

func TestProxmoxProvider_CreateAndDescribe(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Test VM creation
	createReq := &providerv1.CreateRequest{
		Name: "test-vm-create",
		ClassJson: `{"cpus": 2, "memory": "2Gi"}`,
		ImageJson: `{"source": "ubuntu-22-template"}`,
		UserData:  []byte("#cloud-config\nhostname: test-vm"),
	}

	createResp, err := provider.Create(ctx, createReq)
	require.NoError(t, err)
	require.NotEmpty(t, createResp.Id)

	// Wait for creation task to complete if there is one
	if createResp.Task != nil {
		err = waitForTask(ctx, provider, createResp.Task.Id)
		require.NoError(t, err)
	}

	// Test VM description
	describeReq := &providerv1.DescribeRequest{
		Id: createResp.Id,
	}

	describeResp, err := provider.Describe(ctx, describeReq)
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
	assert.Equal(t, "off", describeResp.PowerState)
	assert.NotEmpty(t, describeResp.ConsoleUrl)
}

func TestProxmoxProvider_PowerOperations(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use existing test VM (ID 100 from fake server seed data)
	vmID := "100"

	// Test power on
	powerReq := &providerv1.PowerRequest{
		Id: vmID,
		Op: providerv1.PowerOp_POWER_OP_ON,
	}

	powerResp, err := provider.Power(ctx, powerReq)
	require.NoError(t, err)

	// Wait for power task to complete if there is one
	if powerResp.Task != nil {
		err = waitForTask(ctx, provider, powerResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM is running
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: vmID})
	require.NoError(t, err)
	assert.Equal(t, "on", describeResp.PowerState)

	// Test power off
	powerReq.Op = providerv1.PowerOp_POWER_OP_OFF
	powerResp, err = provider.Power(ctx, powerReq)
	require.NoError(t, err)

	// Wait for power task to complete if there is one
	if powerResp.Task != nil {
		err = waitForTask(ctx, provider, powerResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM is stopped
	describeResp, err = provider.Describe(ctx, &providerv1.DescribeRequest{Id: vmID})
	require.NoError(t, err)
	assert.Equal(t, "off", describeResp.PowerState)
}

func TestProxmoxProvider_Clone(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Use template VM (ID 9000 from fake server seed data)
	cloneReq := &providerv1.CloneRequest{
		SourceVmId:  "9000",
		TargetName:  "test-clone",
		LinkedClone: true,
	}

	cloneResp, err := provider.Clone(ctx, cloneReq)
	require.NoError(t, err)
	require.NotEmpty(t, cloneResp.TargetVmId)

	// Wait for clone task to complete if there is one
	if cloneResp.Task != nil {
		err = waitForTask(ctx, provider, cloneResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify cloned VM exists
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: cloneResp.TargetVmId})
	require.NoError(t, err)
	assert.True(t, describeResp.Exists)
}

func TestProxmoxProvider_Delete(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()

	// Create a VM first
	createReq := &providerv1.CreateRequest{
		Name:      "test-vm-delete",
		ClassJson: `{"cpus": 1, "memory": "1Gi"}`,
	}

	createResp, err := provider.Create(ctx, createReq)
	require.NoError(t, err)

	// Wait for creation task to complete if there is one
	if createResp.Task != nil {
		err = waitForTask(ctx, provider, createResp.Task.Id)
		require.NoError(t, err)
	}

	// Delete the VM
	deleteReq := &providerv1.DeleteRequest{
		Id: createResp.Id,
	}

	deleteResp, err := provider.Delete(ctx, deleteReq)
	require.NoError(t, err)

	// Wait for deletion task to complete if there is one
	if deleteResp.Task != nil {
		err = waitForTask(ctx, provider, deleteResp.Task.Id)
		require.NoError(t, err)
	}

	// Verify VM no longer exists
	describeResp, err := provider.Describe(ctx, &providerv1.DescribeRequest{Id: createResp.Id})
	require.NoError(t, err)
	assert.False(t, describeResp.Exists)
}

func TestProxmoxProvider_GetCapabilities(t *testing.T) {
	// Start fake PVE server
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)

	// Create provider with fake server
	provider := createTestProvider(endpoint)

	ctx := context.Background()
	resp, err := provider.GetCapabilities(ctx, &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	assert.True(t, resp.SupportsSnapshots)
	assert.True(t, resp.SupportsLinkedClones)
	assert.True(t, resp.SupportsImageImport)
	assert.Contains(t, resp.SupportedDiskTypes, "raw")
	assert.Contains(t, resp.SupportedDiskTypes, "qcow2")
	assert.Contains(t, resp.SupportedNetworkTypes, "bridge")
}

// Helper functions

func createTestProvider(endpoint string) *Provider {
	config := &pveapi.Config{
		Endpoint:           endpoint,
		TokenID:            "test@pve!token",
		TokenSecret:        "secret",
		InsecureSkipVerify: true,
	}

	client, err := pveapi.NewClient(config)
	if err != nil {
		panic(err)
	}

	provider := New()
	provider.client = client

	return provider
}

func waitForTask(ctx context.Context, provider *Provider, taskID string) error {
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return context.DeadlineExceeded
		case <-ticker.C:
			resp, err := provider.TaskStatus(ctx, &providerv1.TaskStatusRequest{
				Task: &providerv1.TaskRef{Id: taskID},
			})
			if err != nil {
				return err
			}
			if resp.Done {
				if resp.Error != "" {
					return fmt.Errorf("task failed: %s", resp.Error)
				}
				return nil
			}
		}
	}
}