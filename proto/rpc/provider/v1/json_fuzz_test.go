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
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NOTE: this file previously fuzzed CreateVMRequest/VMSpec/DiskSpec/
// NetworkSpec/PowerVMRequest/CreateVMResponse — none of which exist in the
// current schema, so `go vet ./...` failed with `undefined: CreateVMRequest`
// and the file compiled zero coverage. It has been rewritten against the
// current generated types. The schema shape also changed materially: a
// CreateRequest no longer nests typed VMSpec/DiskSpec/NetworkSpec messages,
// it carries the spec as opaque, pre-rendered JSON strings (class_json,
// image_json, networks_json, disks_json, placement_json) that protojson
// treats as plain strings. VMInfo/DiskInfo/NetworkInfo is the closest
// current analog for nested-message and map-field coverage.

// allValidUTF8 reports whether every string is valid UTF-8. proto3 `string`
// fields (unlike `bytes`) are defined to always hold UTF-8 text, and
// protojson.Marshal correctly rejects a message that violates that
// invariant. Go's native fuzzer generates arbitrary byte sequences for a
// `string` parameter with no such guarantee, so any fuzz case that assigns a
// fuzzed string directly into a proto string field must skip non-UTF-8
// input first -- otherwise the test "fails" on a fuzzer artifact rather than
// a real bug.
func allValidUTF8(strs ...string) bool {
	for _, s := range strs {
		if !utf8.ValidString(s) {
			return false
		}
	}
	return true
}

// FuzzCreateRequestJSON tests JSON marshaling/unmarshaling of CreateRequest.
// Its *_json fields are opaque, pre-rendered JSON payloads carried as plain
// proto strings -- protojson must round-trip them byte-for-byte without
// attempting to interpret their contents, even when they are not valid JSON
// themselves.
func FuzzCreateRequestJSON(f *testing.F) {
	f.Add("test-vm", "ubuntu:20.04", "vsphere-cluster", `{"cpu":2,"memoryMiB":4096}`)
	f.Add("", "", "", "")
	f.Add("fuzzy-vm-名前", "windows/server:2019", "special-cluster", `{"broken`)

	f.Fuzz(func(t *testing.T, name, image, cluster, classJSON string) {
		if !allValidUTF8(name, image, cluster, classJSON) {
			t.Skip("fuzz-generated string is not valid UTF-8")
		}

		original := &CreateRequest{
			Name:          name,
			UserData:      []byte(name),
			ClassJson:     classJSON,
			ImageJson:     image,
			PlacementJson: cluster,
			Tags:          []string{"env:test"},
		}

		protoJSONBytes, err := protojson.Marshal(original)
		if err != nil {
			t.Fatalf("failed to marshal to protojson: %v", err)
		}

		roundTripped := &CreateRequest{}
		if err := protojson.Unmarshal(protoJSONBytes, roundTripped); err != nil {
			t.Fatalf("failed to unmarshal from protojson: %v", err)
		}

		if !proto.Equal(original, roundTripped) {
			t.Errorf("protojson round-trip failed:\noriginal: %+v\nunmarshaled: %+v", original, roundTripped)
		}

		// name/image/cluster must also survive a plain encoding/json
		// round-trip when carried in an ordinary map, since callers
		// sometimes log or cache these values outside protojson.
		standardJSONBytes, err := json.Marshal(map[string]string{
			"name":    name,
			"image":   image,
			"cluster": cluster,
		})
		if err != nil {
			t.Fatalf("failed to marshal to standard JSON: %v", err)
		}

		var jsonData map[string]string
		if err := json.Unmarshal(standardJSONBytes, &jsonData); err != nil {
			t.Fatalf("failed to unmarshal standard JSON: %v", err)
		}
		if jsonData["name"] != name {
			t.Errorf("JSON name mismatch: expected %q, got %q", name, jsonData["name"])
		}
	})
}

// FuzzVMInfoJSON tests JSON serialization of VMInfo, which nests repeated
// DiskInfo/NetworkInfo messages plus a string map (provider_raw) -- the
// closest current analog to the old (now nonexistent) VMSpec message for
// exercising nested-structure and map-field round-trips.
func FuzzVMInfoJSON(f *testing.F) {
	f.Add(int32(4), int64(8192), int32(100), "qcow2", "region", "us-east")
	f.Add(int32(1), int64(512), int32(0), "", "", "")
	f.Add(int32(-1), int64(-1), int32(-1), "raw", "很长的键", "🌟value")

	f.Fuzz(func(t *testing.T, cpu int32, memoryMib int64, diskSizeGib int32, diskFormat, metaKey, metaValue string) {
		if !allValidUTF8(diskFormat, metaKey, metaValue) {
			t.Skip("fuzz-generated string is not valid UTF-8")
		}

		info := &VMInfo{
			Id:         "vm-123",
			Name:       "test-vm",
			PowerState: "poweredOn",
			Cpu:        cpu,
			MemoryMib:  memoryMib,
			Disks: []*DiskInfo{
				{Id: "disk-0", SizeGib: diskSizeGib, Format: diskFormat},
			},
			Networks: []*NetworkInfo{
				{Name: "eth0", Mac: "52:54:00:12:34:56"},
			},
		}
		if metaKey != "" {
			info.ProviderRaw = map[string]string{metaKey: metaValue}
		}

		jsonBytes, err := protojson.Marshal(info)
		if err != nil {
			t.Fatalf("failed to marshal VMInfo: %v", err)
		}

		unmarshaled := &VMInfo{}
		if err := protojson.Unmarshal(jsonBytes, unmarshaled); err != nil {
			t.Fatalf("failed to unmarshal VMInfo: %v", err)
		}

		if !proto.Equal(info, unmarshaled) {
			t.Errorf("VMInfo round-trip failed:\noriginal: %+v\nunmarshaled: %+v", info, unmarshaled)
		}
		if len(unmarshaled.Disks) != len(info.Disks) {
			t.Errorf("disk count mismatch: expected %d, got %d", len(info.Disks), len(unmarshaled.Disks))
		}
		if len(unmarshaled.Disks) > 0 && len(info.Disks) > 0 && unmarshaled.Disks[0].SizeGib != info.Disks[0].SizeGib {
			t.Errorf("disk size mismatch: expected %d, got %d", info.Disks[0].SizeGib, unmarshaled.Disks[0].SizeGib)
		}
		if len(unmarshaled.ProviderRaw) != len(info.ProviderRaw) {
			t.Errorf("provider_raw map size mismatch: expected %d, got %d", len(info.ProviderRaw), len(unmarshaled.ProviderRaw))
		}
	})
}

// FuzzProviderResponseJSON tests JSON serialization of the response types
// returned by the Create and GetCapabilities RPCs, including CreateResponse's
// nested TaskRef message.
func FuzzProviderResponseJSON(f *testing.F) {
	f.Add("vm-123", "task-1", "qcow2", true)
	f.Add("", "", "", false)
	f.Add("很长的虚拟机名字", "🌟task", "🌟fmt", true)

	f.Fuzz(func(t *testing.T, vmID, taskID, diskType string, supportsSnapshots bool) {
		if !allValidUTF8(vmID, taskID, diskType) {
			t.Skip("fuzz-generated string is not valid UTF-8")
		}

		createResp := &CreateResponse{
			Id:   vmID,
			Task: &TaskRef{Id: taskID},
		}

		jsonBytes, err := protojson.Marshal(createResp)
		if err != nil {
			t.Fatalf("failed to marshal CreateResponse: %v", err)
		}

		unmarshaled := &CreateResponse{}
		if err := protojson.Unmarshal(jsonBytes, unmarshaled); err != nil {
			t.Fatalf("failed to unmarshal CreateResponse: %v", err)
		}
		if !proto.Equal(createResp, unmarshaled) {
			t.Errorf("CreateResponse round-trip failed")
		}

		capResp := &GetCapabilitiesResponse{
			SupportsSnapshots:  supportsSnapshots,
			SupportedDiskTypes: []string{diskType},
		}

		jsonBytes, err = protojson.Marshal(capResp)
		if err != nil {
			t.Fatalf("failed to marshal GetCapabilitiesResponse: %v", err)
		}

		capUnmarshaled := &GetCapabilitiesResponse{}
		if err := protojson.Unmarshal(jsonBytes, capUnmarshaled); err != nil {
			t.Fatalf("failed to unmarshal GetCapabilitiesResponse: %v", err)
		}
		if !proto.Equal(capResp, capUnmarshaled) {
			t.Errorf("GetCapabilitiesResponse round-trip failed")
		}
	})
}

// FuzzEnumFieldsJSON tests JSON serialization of PowerRequest's PowerOp
// enum field, including values outside the declared enum range -- proto3
// enums are open, so unrecognized numbers must still round-trip losslessly.
func FuzzEnumFieldsJSON(f *testing.F) {
	f.Add("vm-1", int32(0))
	f.Add("vm-1", int32(1))
	f.Add("vm-1", int32(999)) // out-of-range enum value

	f.Fuzz(func(t *testing.T, id string, powerOp int32) {
		if !allValidUTF8(id) {
			t.Skip("fuzz-generated string is not valid UTF-8")
		}

		powerReq := &PowerRequest{
			Id: id,
			Op: PowerOp(powerOp),
		}

		jsonBytes, err := protojson.Marshal(powerReq)
		if err != nil {
			t.Fatalf("failed to marshal PowerRequest with enum: %v", err)
		}

		unmarshaled := &PowerRequest{}
		if err := protojson.Unmarshal(jsonBytes, unmarshaled); err != nil {
			t.Fatalf("failed to unmarshal PowerRequest with enum: %v", err)
		}
		if !proto.Equal(powerReq, unmarshaled) {
			t.Errorf("PowerRequest with enum round-trip failed:\noriginal: %+v\nunmarshaled: %+v", powerReq, unmarshaled)
		}
	})
}

// FuzzMalformedJSON tests resilience against malformed JSON input across a
// representative sample of the generated message types: scalar-only
// (PowerRequest), opaque-JSON-string (CreateRequest), nested/map
// (VMInfo), flat-bool-heavy (GetCapabilitiesResponse), and an
// otherwise-untouched response type (DescribeResponse). Most malformed
// input is expected to fail to unmarshal; it must never panic.
func FuzzMalformedJSON(f *testing.F) {
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
		messages := []proto.Message{
			&CreateRequest{},
			&VMInfo{},
			&GetCapabilitiesResponse{},
			&PowerRequest{},
			&DescribeResponse{},
		}

		for _, msg := range messages {
			err := protojson.Unmarshal([]byte(jsonInput), msg)
			if err == nil {
				// If it succeeded, marshaling it back out must also succeed.
				if _, marshalErr := protojson.Marshal(msg); marshalErr != nil {
					t.Errorf("successfully unmarshaled malformed JSON into %T but failed to marshal back: %v", msg, marshalErr)
				}
			}
		}
	})
}

// FuzzLargeJSON tests behavior with very large JSON payloads.
func FuzzLargeJSON(f *testing.F) {
	f.Add(100, 1000) // 100 disks, 1000 char strings
	f.Add(10, 10000) // 10 disks, 10000 char strings
	f.Add(1000, 100) // 1000 disks, 100 char strings

	f.Fuzz(func(t *testing.T, diskCount, stringLength int) {
		// Limit to reasonable sizes to avoid timeouts.
		if diskCount > 1000 || stringLength > 10000 || diskCount < 0 || stringLength < 0 {
			t.Skip("skipping unreasonable sizes")
		}

		info := &VMInfo{
			Id:   "vm-large",
			Name: generateString("name-", stringLength),
			Cpu:  8,
		}
		for i := 0; i < diskCount; i++ {
			info.Disks = append(info.Disks, &DiskInfo{
				Id:      generateString("disk-", stringLength/10),
				SizeGib: int32(i + 1),
			})
		}

		jsonBytes, err := protojson.Marshal(info)
		if err != nil {
			t.Fatalf("failed to marshal large VMInfo: %v", err)
		}
		if len(jsonBytes) == 0 {
			t.Error("marshaled JSON is empty")
		}

		unmarshaled := &VMInfo{}
		if err := protojson.Unmarshal(jsonBytes, unmarshaled); err != nil {
			t.Fatalf("failed to unmarshal large VMInfo: %v", err)
		}
		if len(unmarshaled.Disks) != len(info.Disks) {
			t.Errorf("disk count mismatch in large JSON: expected %d, got %d", len(info.Disks), len(unmarshaled.Disks))
		}
		if unmarshaled.Cpu != info.Cpu {
			t.Errorf("cpu mismatch in large JSON: expected %d, got %d", info.Cpu, unmarshaled.Cpu)
		}
	})
}

// generateString creates a string with the given prefix and target length.
func generateString(prefix string, targetLength int) string {
	if targetLength <= len(prefix) {
		return prefix[:targetLength]
	}

	result := prefix
	remaining := targetLength - len(prefix)

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

// FuzzJSONFieldNames tests handling of various JSON field name cases when
// unmarshaling into CreateRequest: protojson accepts both the proto field
// name (snake_case, e.g. "class_json") and its JSON name (camelCase, e.g.
// "classJson", protojson's default Marshal output); other casings are not
// guaranteed to match and are expected to error rather than panic.
func FuzzJSONFieldNames(f *testing.F) {
	f.Add(`{"name": "test", "classJson": "{}"}`)   // camelCase (protojson default output)
	f.Add(`{"name": "test", "class_json": "{}"}`)  // snake_case (proto field name)
	f.Add(`{"Name": "test", "ClassJson": "{}"}`)   // PascalCase
	f.Add(`{"name": "test", "class-json": "{}"}`)  // kebab-case
	f.Add(`{"userData": "dGVzdA==", "name": "x"}`) // camelCase bytes field

	f.Fuzz(func(t *testing.T, jsonInput string) {
		req := &CreateRequest{}
		err := protojson.Unmarshal([]byte(jsonInput), req)
		if err != nil {
			t.Logf("expected field name variation failure: %v", err)
			return
		}

		if _, err := protojson.Marshal(req); err != nil {
			t.Errorf("successfully unmarshaled field name variation but failed to marshal back: %v", err)
		}
	})
}

// FuzzJSONWithUnicodeContent tests handling of Unicode content across a
// scalar-string message (CreateRequest) and a nested-message field
// (VMInfo.Networks[].Name).
func FuzzJSONWithUnicodeContent(f *testing.F) {
	f.Add("🚀 rocket vm", "eth0", "💾 image")
	f.Add("虚拟机", "网络", "存储")
	f.Add("виртуальная машина", "сеть", "хранилище")
	f.Add("máquina virtual", "red", "almacenamiento")

	f.Fuzz(func(t *testing.T, vmName, networkName, imageRef string) {
		if !allValidUTF8(vmName, networkName, imageRef) {
			t.Skip("fuzz-generated string is not valid UTF-8")
		}

		info := &VMInfo{
			Name:     vmName,
			Networks: []*NetworkInfo{{Name: networkName}},
		}
		req := &CreateRequest{
			Name:      vmName,
			ImageJson: imageRef,
			Tags:      []string{vmName},
		}

		for _, msg := range []proto.Message{info, req} {
			jsonBytes, err := protojson.Marshal(msg)
			if err != nil {
				t.Fatalf("failed to marshal Unicode content for %T: %v", msg, err)
			}

			var jsonObj map[string]any
			if err := json.Unmarshal(jsonBytes, &jsonObj); err != nil {
				t.Fatalf("generated invalid JSON with Unicode content for %T: %v", msg, err)
			}
		}

		unmarshaledInfo := &VMInfo{}
		infoBytes, err := protojson.Marshal(info)
		if err != nil {
			t.Fatalf("failed to marshal Unicode VMInfo: %v", err)
		}
		if err := protojson.Unmarshal(infoBytes, unmarshaledInfo); err != nil {
			t.Fatalf("failed to unmarshal Unicode VMInfo: %v", err)
		}
		if unmarshaledInfo.Name != vmName {
			t.Errorf("Unicode VM name not preserved: expected %q, got %q", vmName, unmarshaledInfo.Name)
		}
		if len(unmarshaledInfo.Networks) > 0 && unmarshaledInfo.Networks[0].Name != networkName {
			t.Errorf("Unicode network name not preserved: expected %q, got %q", networkName, unmarshaledInfo.Networks[0].Name)
		}

		unmarshaledReq := &CreateRequest{}
		reqBytes, err := protojson.Marshal(req)
		if err != nil {
			t.Fatalf("failed to marshal Unicode CreateRequest: %v", err)
		}
		if err := protojson.Unmarshal(reqBytes, unmarshaledReq); err != nil {
			t.Fatalf("failed to unmarshal Unicode CreateRequest: %v", err)
		}
		if unmarshaledReq.Name != vmName {
			t.Errorf("Unicode CreateRequest name not preserved: expected %q, got %q", vmName, unmarshaledReq.Name)
		}
	})
}
