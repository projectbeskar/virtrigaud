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

package providerv1

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// FuzzCreateVMRequestJSON tests JSON marshaling/unmarshaling of CreateVMRequest
func FuzzCreateVMRequestJSON(f *testing.F) {
	// Add seed inputs
	f.Add("test-vm", int32(2), int32(4096), "ubuntu:20.04", "vsphere-cluster")
	f.Add("", int32(0), int32(0), "", "")
	f.Add("fuzzy-vm-名前", int32(999), int32(999999), "windows/server:2019", "special-cluster")

	f.Fuzz(func(t *testing.T, name string, cpu, memory int32, image, cluster string) {
		// Skip obviously invalid values that would cause protobuf issues
		if cpu < 0 || memory < 0 {
			t.Skip("Invalid CPU or memory values")
		}

		// Create original request
		original := &CreateVMRequest{
			Name: name,
			Spec: &VMSpec{
				Cpu:    cpu,
				Memory: memory,
				Image:  image,
			},
			Cluster: cluster,
		}

		// Test protojson marshaling
		protoJSONBytes, err := protojson.Marshal(original)
		if err != nil {
			t.Fatalf("Failed to marshal to protojson: %v", err)
		}

		// Test protojson unmarshaling
		protoJSONUnmarshaled := &CreateVMRequest{}
		err = protojson.Unmarshal(protoJSONBytes, protoJSONUnmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal from protojson: %v", err)
		}

		// Verify protojson round-trip
		if !proto.Equal(original, protoJSONUnmarshaled) {
			t.Errorf("protojson round-trip failed:\noriginal: %+v\nunmarshaled: %+v", original, protoJSONUnmarshaled)
		}

		// Test standard JSON marshaling (should work for simple fields)
		standardJSONBytes, err := json.Marshal(map[string]interface{}{
			"name":    name,
			"cpu":     cpu,
			"memory":  memory,
			"image":   image,
			"cluster": cluster,
		})
		if err != nil {
			t.Fatalf("Failed to marshal to standard JSON: %v", err)
		}

		// Verify JSON is valid
		var jsonData map[string]interface{}
		err = json.Unmarshal(standardJSONBytes, &jsonData)
		if err != nil {
			t.Fatalf("Failed to unmarshal standard JSON: %v", err)
		}

		// Basic validation of JSON structure
		if name != "" {
			if jsonData["name"] != name {
				t.Errorf("JSON name mismatch: expected %s, got %v", name, jsonData["name"])
			}
		}
	})
}

// FuzzVMSpecJSON tests JSON serialization of VMSpec with complex nested structures
func FuzzVMSpecJSON(f *testing.F) {
	f.Add(int32(4), int32(8192), "centos:8", int32(100), "ext4")
	f.Add(int32(1), int32(512), "", int32(0), "")

	f.Fuzz(func(t *testing.T, cpu, memory int32, image string, diskSize int32, diskType string) {
		if cpu < 0 || memory < 0 || diskSize < 0 {
			t.Skip("Invalid resource values")
		}

		// Create VMSpec with nested structures
		spec := &VMSpec{
			Cpu:    cpu,
			Memory: memory,
			Image:  image,
			Disks: []*DiskSpec{
				{
					Size: diskSize,
					Type: diskType,
				},
			},
			Network: &NetworkSpec{
				Interfaces: []*NetworkInterface{
					{
						Name: "eth0",
						Type: "vmxnet3",
					},
				},
			},
		}

		// Test protojson serialization
		jsonBytes, err := protojson.Marshal(spec)
		if err != nil {
			t.Fatalf("Failed to marshal VMSpec: %v", err)
		}

		// Test deserialization
		unmarshaled := &VMSpec{}
		err = protojson.Unmarshal(jsonBytes, unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal VMSpec: %v", err)
		}

		// Verify round-trip equality
		if !proto.Equal(spec, unmarshaled) {
			t.Errorf("VMSpec round-trip failed:\noriginal: %+v\nunmarshaled: %+v", spec, unmarshaled)
		}

		// Test nested structure preservation
		if len(unmarshaled.Disks) != len(spec.Disks) {
			t.Errorf("Disk count mismatch: expected %d, got %d", len(spec.Disks), len(unmarshaled.Disks))
		}

		if len(unmarshaled.Disks) > 0 && len(spec.Disks) > 0 {
			if unmarshaled.Disks[0].Size != spec.Disks[0].Size {
				t.Errorf("Disk size mismatch: expected %d, got %d", spec.Disks[0].Size, unmarshaled.Disks[0].Size)
			}
		}
	})
}

// FuzzProviderResponseJSON tests JSON serialization of various response types
func FuzzProviderResponseJSON(f *testing.F) {
	f.Add("vm-123", "Running", "provider-1", "Operation completed successfully")
	f.Add("", "Unknown", "", "")
	f.Add("很长的虚拟机名字", "Failed", "🌟provider", "Error: 💥 Something went wrong!")

	f.Fuzz(func(t *testing.T, vmID, state, providerID, message string) {
		// Test CreateVMResponse
		createResp := &CreateVMResponse{
			VmId: vmID,
			Status: &VMStatus{
				State:   state,
				Message: message,
			},
		}

		jsonBytes, err := protojson.Marshal(createResp)
		if err != nil {
			t.Fatalf("Failed to marshal CreateVMResponse: %v", err)
		}

		unmarshaled := &CreateVMResponse{}
		err = protojson.Unmarshal(jsonBytes, unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal CreateVMResponse: %v", err)
		}

		if !proto.Equal(createResp, unmarshaled) {
			t.Errorf("CreateVMResponse round-trip failed")
		}

		// Test GetCapabilitiesResponse
		capResp := &GetCapabilitiesResponse{
			ProviderId: providerID,
			Capabilities: []*Capability{
				{
					Name:        "vm.create",
					Supported:   true,
					Description: message,
				},
			},
		}

		jsonBytes, err = protojson.Marshal(capResp)
		if err != nil {
			t.Fatalf("Failed to marshal GetCapabilitiesResponse: %v", err)
		}

		capUnmarshaled := &GetCapabilitiesResponse{}
		err = protojson.Unmarshal(jsonBytes, capUnmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal GetCapabilitiesResponse: %v", err)
		}

		if !proto.Equal(capResp, capUnmarshaled) {
			t.Errorf("GetCapabilitiesResponse round-trip failed")
		}
	})
}

// FuzzEnumFieldsJSON tests JSON serialization of protobuf enum fields
func FuzzEnumFieldsJSON(f *testing.F) {
	f.Add(int32(0), int32(1), int32(2)) // PowerOp values
	f.Add(int32(999), int32(-1), int32(100)) // Invalid enum values

	f.Fuzz(func(t *testing.T, powerOp, vmState, taskState int32) {
		// Create request with enum fields
		powerReq := &PowerVMRequest{
			VmId:     "test-vm",
			PowerOp:  PowerOp(powerOp),
		}

		// Test JSON serialization of enums
		jsonBytes, err := protojson.Marshal(powerReq)
		if err != nil {
			t.Fatalf("Failed to marshal PowerVMRequest with enum: %v", err)
		}

		// Test deserialization
		unmarshaled := &PowerVMRequest{}
		err = protojson.Unmarshal(jsonBytes, unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal PowerVMRequest with enum: %v", err)
		}

		// Verify enum handling
		if !proto.Equal(powerReq, unmarshaled) {
			t.Errorf("PowerVMRequest with enum round-trip failed:\noriginal: %+v\nunmarshaled: %+v", powerReq, unmarshaled)
		}

		// Test that enum values are preserved or properly handled
		if unmarshaled.PowerOp != powerReq.PowerOp {
			// This might be expected for invalid enum values
			t.Logf("Enum value changed during serialization: %v -> %v (may be expected for invalid values)", 
				powerReq.PowerOp, unmarshaled.PowerOp)
		}
	})
}

// FuzzMalformedJSON tests resilience against malformed JSON input
func FuzzMalformedJSON(f *testing.F) {
	// Add various malformed JSON examples
	f.Add(`{"name": "test"`)                    // Missing closing brace
	f.Add(`{"name": test"}`)                    // Missing opening quote
	f.Add(`{"name": "test", "cpu": "not-int"}`) // Wrong type
	f.Add(`{"name": null}`)                     // Null values
	f.Add(`{}`)                                 // Empty object
	f.Add(`[]`)                                 // Array instead of object
	f.Add(`"just-a-string"`)                    // Plain string
	f.Add(`123`)                                // Number
	f.Add(`true`)                               // Boolean

	f.Fuzz(func(t *testing.T, jsonInput string) {
		// Try to unmarshal into various message types
		messages := []proto.Message{
			&CreateVMRequest{},
			&VMSpec{},
			&GetCapabilitiesResponse{},
			&PowerVMRequest{},
			&VMStatus{},
		}

		for _, msg := range messages {
			err := protojson.Unmarshal([]byte(jsonInput), msg)
			// We expect most malformed JSON to fail, but it shouldn't panic
			if err == nil {
				t.Logf("Unexpectedly successful unmarshal of %q into %T", jsonInput, msg)
				
				// If it succeeded, try to marshal it back
				_, marshalErr := protojson.Marshal(msg)
				if marshalErr != nil {
					t.Errorf("Successfully unmarshaled malformed JSON but failed to marshal back: %v", marshalErr)
				}
			}
		}
	})
}

// FuzzLargeJSON tests behavior with very large JSON payloads
func FuzzLargeJSON(f *testing.F) {
	f.Add(100, 1000)   // 100 disks, 1000 char strings
	f.Add(10, 10000)   // 10 disks, 10000 char strings  
	f.Add(1000, 100)   // 1000 disks, 100 char strings

	f.Fuzz(func(t *testing.T, diskCount, stringLength int) {
		// Limit to reasonable sizes to avoid timeouts
		if diskCount > 1000 || stringLength > 10000 || diskCount < 0 || stringLength < 0 {
			t.Skip("Skipping unreasonable sizes")
		}

		// Create large VMSpec
		spec := &VMSpec{
			Cpu:    8,
			Memory: 16384,
			Image:  generateString("image-", stringLength),
		}

		// Add many disks
		for i := 0; i < diskCount; i++ {
			spec.Disks = append(spec.Disks, &DiskSpec{
				Size: int32(i + 1),
				Type: generateString("disk-type-", stringLength/10),
			})
		}

		// Test serialization performance and correctness
		jsonBytes, err := protojson.Marshal(spec)
		if err != nil {
			t.Fatalf("Failed to marshal large VMSpec: %v", err)
		}

		// Verify size is reasonable (basic sanity check)
		if len(jsonBytes) == 0 {
			t.Error("Marshaled JSON is empty")
		}

		// Test deserialization
		unmarshaled := &VMSpec{}
		err = protojson.Unmarshal(jsonBytes, unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal large VMSpec: %v", err)
		}

		// Verify structure
		if len(unmarshaled.Disks) != len(spec.Disks) {
			t.Errorf("Disk count mismatch in large JSON: expected %d, got %d", len(spec.Disks), len(unmarshaled.Disks))
		}

		if unmarshaled.Cpu != spec.Cpu {
			t.Errorf("CPU mismatch in large JSON: expected %d, got %d", spec.Cpu, unmarshaled.Cpu)
		}
	})
}

// generateString creates a string with the given prefix and target length
func generateString(prefix string, targetLength int) string {
	if targetLength <= len(prefix) {
		return prefix[:targetLength]
	}
	
	result := prefix
	remaining := targetLength - len(prefix)
	
	// Fill with repeating pattern
	pattern := "abcdefghijklmnopqrstuvwxyz0123456789"
	for len(result) < targetLength {
		if remaining < len(pattern) {
			result += pattern[:remaining]
			break
		}
		result += pattern
		remaining -= len(pattern)
	}
	
	return result
}

// FuzzJSONFieldNames tests handling of various JSON field name cases
func FuzzJSONFieldNames(f *testing.F) {
	f.Add(`{"name": "test", "cpu": 2}`)                    // Standard camelCase
	f.Add(`{"Name": "test", "CPU": 2}`)                    // PascalCase
	f.Add(`{"vm_id": "test", "cpu_count": 2}`)             // snake_case
	f.Add(`{"vm-id": "test", "cpu-count": 2}`)             // kebab-case
	f.Add(`{"vmId": "test", "cpuCount": 2}`)               // camelCase variant

	f.Fuzz(func(t *testing.T, jsonInput string) {
		// Test with CreateVMRequest which has various field types
		req := &CreateVMRequest{}
		err := protojson.Unmarshal([]byte(jsonInput), req)
		
		// Field name variations might not all work, but shouldn't panic
		if err != nil {
			t.Logf("Expected field name variation failure: %v", err)
			return
		}

		// If successful, verify we can marshal back
		_, err = protojson.Marshal(req)
		if err != nil {
			t.Errorf("Successfully unmarshaled field name variation but failed to marshal back: %v", err)
		}
	})
}

// FuzzJSONWithUnicodeContent tests handling of Unicode content in JSON
func FuzzJSONWithUnicodeContent(f *testing.F) {
	f.Add("🚀 rocket vm", "💾 storage", "🌐 network")
	f.Add("虚拟机", "存储", "网络")
	f.Add("виртуальная машина", "хранилище", "сеть")
	f.Add("máquina virtual", "almacenamiento", "red")

	f.Fuzz(func(t *testing.T, vmName, diskType, networkName string) {
		// Create spec with Unicode content
		spec := &VMSpec{
			Cpu:    2,
			Memory: 4096,
			Disks: []*DiskSpec{
				{
					Type: diskType,
					Size: 100,
				},
			},
			Network: &NetworkSpec{
				Interfaces: []*NetworkInterface{
					{
						Name: networkName,
						Type: "virtio",
					},
				},
			},
		}

		req := &CreateVMRequest{
			Name: vmName,
			Spec: spec,
		}

		// Test Unicode handling in JSON
		jsonBytes, err := protojson.Marshal(req)
		if err != nil {
			t.Fatalf("Failed to marshal Unicode content: %v", err)
		}

		// Verify JSON is valid
		var jsonObj map[string]interface{}
		err = json.Unmarshal(jsonBytes, &jsonObj)
		if err != nil {
			t.Fatalf("Generated invalid JSON with Unicode content: %v", err)
		}

		// Test round-trip
		unmarshaled := &CreateVMRequest{}
		err = protojson.Unmarshal(jsonBytes, unmarshaled)
		if err != nil {
			t.Fatalf("Failed to unmarshal Unicode content: %v", err)
		}

		// Verify Unicode content preservation
		if unmarshaled.Name != vmName {
			t.Errorf("Unicode VM name not preserved: expected %q, got %q", vmName, unmarshaled.Name)
		}

		if len(unmarshaled.Spec.Disks) > 0 && unmarshaled.Spec.Disks[0].Type != diskType {
			t.Errorf("Unicode disk type not preserved: expected %q, got %q", diskType, unmarshaled.Spec.Disks[0].Type)
		}
	})
}

