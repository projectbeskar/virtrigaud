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

package v1alpha1

import (
	"testing"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// FuzzVirtualMachineConversion tests round-trip conversion between v1alpha1 and v1beta1 VirtualMachine objects
func FuzzVirtualMachineConversion(f *testing.F) {
	// Add seed inputs for the fuzzer
	f.Add("test-vm", "On", "Running", int32(2), int32(4096), "ubuntu:20.04")
	f.Add("", "Off", "Stopped", int32(1), int32(1024), "")
	f.Add("fuzzy-vm", "Restart", "Pending", int32(8), int32(16384), "windows:2019")

	f.Fuzz(func(t *testing.T, name, powerState, phase string, cpu, memory int32, image string) {
		// Skip invalid inputs that would cause panics
		if cpu < 0 || memory < 0 {
			t.Skip("Invalid CPU or memory values")
		}

		// Create v1alpha1 VirtualMachine with fuzzed data
		alpha := &VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: VirtualMachineSpec{
				PowerState: powerState,
				ProviderRef: ObjectRef{Name: "test-provider"},
				ClassRef:    ObjectRef{Name: "test-class"},
				ImageRef:    ObjectRef{Name: image},
				   Resources: &VirtualMachineResources{
					   CPU:       &cpu,
					   MemoryMiB: func(m int32) *int64 { v := int64(m); return &v }(memory),
				   },
			},
			Status: VirtualMachineStatus{
				PowerState: phase,
			},
		}

		// Convert to v1beta1
		beta := &v1beta1.VirtualMachine{}
		err := alpha.ConvertTo(beta)
		if err != nil {
			t.Fatalf("Failed to convert alpha to beta: %v", err)
		}

		// Convert back to v1alpha1
		alphaRoundTrip := &VirtualMachine{}
		err = alphaRoundTrip.ConvertFrom(beta)
		if err != nil {
			t.Fatalf("Failed to convert beta back to alpha: %v", err)
		}

		// Verify lossless conversion for core fields
		if alphaRoundTrip.ObjectMeta.Name != alpha.ObjectMeta.Name {
			t.Errorf("Name mismatch: original=%s, roundtrip=%s", alpha.ObjectMeta.Name, alphaRoundTrip.ObjectMeta.Name)
		}

		if alphaRoundTrip.Spec.Resources != nil && alpha.Spec.Resources != nil {
			if alphaRoundTrip.Spec.Resources.CPU != nil && alpha.Spec.Resources.CPU != nil {
				if *alphaRoundTrip.Spec.Resources.CPU != *alpha.Spec.Resources.CPU {
					t.Errorf("CPU mismatch: original=%d, roundtrip=%d", *alpha.Spec.Resources.CPU, *alphaRoundTrip.Spec.Resources.CPU)
				}
			}
			if alphaRoundTrip.Spec.Resources.MemoryMiB != nil && alpha.Spec.Resources.MemoryMiB != nil {
				if *alphaRoundTrip.Spec.Resources.MemoryMiB != *alpha.Spec.Resources.MemoryMiB {
					t.Errorf("Memory mismatch: original=%d, roundtrip=%d", *alpha.Spec.Resources.MemoryMiB, *alphaRoundTrip.Spec.Resources.MemoryMiB)
				}
			}
		}

		if alphaRoundTrip.Spec.ImageRef.Name != alpha.Spec.ImageRef.Name {
			t.Errorf("Image mismatch: original=%s, roundtrip=%s", alpha.Spec.ImageRef.Name, alphaRoundTrip.Spec.ImageRef.Name)
		}

		// PowerState should be preserved unless it was invalid
		validPowerStates := map[string]bool{
			"On":      true,
			"Off":     true,
			"Restart": true,
			"Suspend": true,
		}

		if validPowerStates[alpha.Spec.PowerState] {
			if alphaRoundTrip.Spec.PowerState != alpha.Spec.PowerState {
				t.Errorf("PowerState mismatch: original=%s, roundtrip=%s", alpha.Spec.PowerState, alphaRoundTrip.Spec.PowerState)
			}
		}

		// PowerState in status should be preserved unless it was invalid
		validStatusPowerStates := map[string]bool{
			"On":      true,
			"Off":     true,
			"Restart": true,
			"Suspend": true,
		}

		if validStatusPowerStates[alpha.Status.PowerState] {
			if alphaRoundTrip.Status.PowerState != alpha.Status.PowerState {
				t.Errorf("Status PowerState mismatch: original=%s, roundtrip=%s", alpha.Status.PowerState, alphaRoundTrip.Status.PowerState)
			}
		}
	})
}

// FuzzVMSetConversion tests round-trip conversion between v1alpha1 and v1beta1 VMSet objects
func FuzzVMSetConversion(f *testing.F) {
	// Add seed inputs for the fuzzer
	f.Add("test-vmset", int32(3), "On", int32(2), int32(4096), "ubuntu:20.04")
	f.Add("", int32(1), "Off", int32(1), int32(1024), "")
	f.Add("large-vmset", int32(10), "Restart", int32(4), int32(8192), "centos:8")

	f.Fuzz(func(t *testing.T, name string, replicas int32, powerState string, cpu, memory int32, image string) {
		// Skip invalid inputs
		if replicas < 0 || cpu < 0 || memory < 0 {
			t.Skip("Invalid replica, CPU, or memory values")
		}

		// Create v1alpha1 VMSet with fuzzed data
		alpha := &VMSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: VMSetSpec{
				Replicas: &replicas,
				Template: VMSetTemplate{
					Spec: VirtualMachineSpec{
						PowerState: powerState,
						Resources: &VirtualMachineResources{
							CPU:       &cpu,
							MemoryMiB: func(m int32) *int64 { v := int64(m); return &v }(memory),
						},
						ImageRef: ObjectRef{
							Name: image,
						},
					},
				},
			},
		}

		// Convert to v1beta1
		beta := &v1beta1.VMSet{}
		err := alpha.ConvertTo(beta)
		if err != nil {
			t.Fatalf("Failed to convert alpha VMSet to beta: %v", err)
		}

		// Convert back to v1alpha1
		alphaRoundTrip := &VMSet{}
		err = alphaRoundTrip.ConvertFrom(beta)
		if err != nil {
			t.Fatalf("Failed to convert beta VMSet back to alpha: %v", err)
		}

		// Verify lossless conversion
		if alphaRoundTrip.ObjectMeta.Name != alpha.ObjectMeta.Name {
			t.Errorf("VMSet name mismatch: original=%s, roundtrip=%s", alpha.ObjectMeta.Name, alphaRoundTrip.ObjectMeta.Name)
		}

		if alphaRoundTrip.Spec.Replicas != nil && alpha.Spec.Replicas != nil {
			if *alphaRoundTrip.Spec.Replicas != *alpha.Spec.Replicas {
				t.Errorf("VMSet replicas mismatch: original=%d, roundtrip=%d", *alpha.Spec.Replicas, *alphaRoundTrip.Spec.Replicas)
			}
		}

		if alphaRoundTrip.Spec.Template.Spec.Resources != nil && alpha.Spec.Template.Spec.Resources != nil {
			if alphaRoundTrip.Spec.Template.Spec.Resources.CPU != nil && alpha.Spec.Template.Spec.Resources.CPU != nil {
				if *alphaRoundTrip.Spec.Template.Spec.Resources.CPU != *alpha.Spec.Template.Spec.Resources.CPU {
					t.Errorf("VMSet template CPU mismatch: original=%d, roundtrip=%d", *alpha.Spec.Template.Spec.Resources.CPU, *alphaRoundTrip.Spec.Template.Spec.Resources.CPU)
				}
			}
			if alphaRoundTrip.Spec.Template.Spec.Resources.MemoryMiB != nil && alpha.Spec.Template.Spec.Resources.MemoryMiB != nil {
				if *alphaRoundTrip.Spec.Template.Spec.Resources.MemoryMiB != *alpha.Spec.Template.Spec.Resources.MemoryMiB {
					t.Errorf("VMSet template Memory mismatch: original=%d, roundtrip=%d", *alpha.Spec.Template.Spec.Resources.MemoryMiB, *alphaRoundTrip.Spec.Template.Spec.Resources.MemoryMiB)
				}
			}
		}

		if alphaRoundTrip.Spec.Template.Spec.ImageRef.Name != alpha.Spec.Template.Spec.ImageRef.Name {
			t.Errorf("VMSet template Image mismatch: original=%s, roundtrip=%s", alpha.Spec.Template.Spec.ImageRef.Name, alphaRoundTrip.Spec.Template.Spec.ImageRef.Name)
		}
	})
}

// FuzzHubConversion tests conversion to/from the hub version (v1beta1)
func FuzzHubConversion(f *testing.F) {
	// Test data for hub conversion
	f.Add("hub-test", "On", int32(4), int32(8192))

	f.Fuzz(func(t *testing.T, name, powerState string, cpu, memory int32) {
		if cpu < 0 || memory < 0 {
			t.Skip("Invalid CPU or memory values")
		}

		// Create v1alpha1 object
		alpha := &VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
			},
			Spec: VirtualMachineSpec{
				PowerState:  powerState,
				ProviderRef: ObjectRef{Name: "test-provider"},
				ClassRef:    ObjectRef{Name: "test-class"},
				ImageRef:    ObjectRef{Name: "test-image"},
				Resources: &VirtualMachineResources{
					CPU:       &cpu,
					MemoryMiB: func(m int32) *int64 { v := int64(m); return &v }(memory),
				},
			},
		}

		// Convert to hub (should be v1beta1)
		hub := &v1beta1.VirtualMachine{}
		err := alpha.ConvertTo(hub)
		if err != nil {
			t.Fatalf("Failed to convert to hub: %v", err)
		}

		// Convert back from hub
		alphaFromHub := &VirtualMachine{}
		err = alphaFromHub.ConvertFrom(hub)
		if err != nil {
			t.Fatalf("Failed to convert from hub: %v", err)
		}

		// Verify round-trip
		if alphaFromHub.ObjectMeta.Name != alpha.ObjectMeta.Name {
			t.Errorf("Hub round-trip name mismatch: original=%s, result=%s", alpha.ObjectMeta.Name, alphaFromHub.ObjectMeta.Name)
		}

		if alphaFromHub.Spec.CPU != alpha.Spec.CPU {
			t.Errorf("Hub round-trip CPU mismatch: original=%d, result=%d", alpha.Spec.CPU, alphaFromHub.Spec.CPU)
		}

		if alphaFromHub.Spec.Memory != alpha.Spec.Memory {
			t.Errorf("Hub round-trip Memory mismatch: original=%d, result=%d", alpha.Spec.Memory, alphaFromHub.Spec.Memory)
		}
	})
}

// FuzzConversionWithNilFields tests conversion behavior with nil/empty fields
func FuzzConversionWithNilFields(f *testing.F) {
	f.Add(true, true, true)   // All fields nil
	f.Add(false, true, false) // Mixed nil fields
	f.Add(false, false, false) // No nil fields

	f.Fuzz(func(t *testing.T, nilReplicas, nilPowerState, nilPhase bool) {
		// Create VMSet with potentially nil fields
		vmset := &VMSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nil-test",
				Namespace: "default",
			},
			Spec: VMSetSpec{
				Template: VMSetTemplate{
					Spec: VirtualMachineSpec{
						ProviderRef: ObjectRef{Name: "test-provider"},
						ClassRef:    ObjectRef{Name: "test-class"},
						ImageRef:    ObjectRef{Name: "test-image"},
						Resources: &VirtualMachineResources{
							CPU:       func() *int32 { v := int32(2); return &v }(),
							MemoryMiB: func() *int64 { v := int64(4096); return &v }(),
						},
					},
				},
			},
		}

		// Conditionally set fields based on fuzz input
		if !nilReplicas {
			replicas := int32(3)
			vmset.Spec.Replicas = &replicas
		}

		if !nilPowerState {
			vmset.Spec.Template.Spec.PowerState = "On"
		}

		if !nilPhase {
			vmset.Status.Phase = "Running"
		}

		// Convert to beta and back
		beta := &v1beta1.VMSet{}
		err := vmset.ConvertTo(beta)
		if err != nil {
			t.Fatalf("Failed to convert with nil fields: %v", err)
		}

		roundTrip := &VMSet{}
		err = roundTrip.ConvertFrom(beta)
		if err != nil {
			t.Fatalf("Failed to convert back with nil fields: %v", err)
		}

		// Verify conversion doesn't panic and handles nil fields gracefully
		// The specific behavior may vary, but conversion should not crash
		if roundTrip.Spec.Template.Spec.CPU != vmset.Spec.Template.Spec.CPU {
			t.Errorf("CPU changed during nil field conversion: original=%d, result=%d", 
				vmset.Spec.Template.Spec.CPU, roundTrip.Spec.Template.Spec.CPU)
		}

		if roundTrip.Spec.Template.Spec.Memory != vmset.Spec.Template.Spec.Memory {
			t.Errorf("Memory changed during nil field conversion: original=%d, result=%d", 
				vmset.Spec.Template.Spec.Memory, roundTrip.Spec.Template.Spec.Memory)
		}
	})
}

// FuzzConversionWithInvalidData tests conversion resilience with invalid data
func FuzzConversionWithInvalidData(f *testing.F) {
	// Test with various invalid inputs
	f.Add("INVALID_POWER", "INVALID_PHASE", int32(-1), int32(-999))
	f.Add("", "", int32(0), int32(0))
	f.Add("超级无敌虚拟机", "🚀", int32(999999), int32(999999))

	f.Fuzz(func(t *testing.T, powerState, phase string, cpu, memory int32) {
		// Create VM with potentially invalid data
		vm := &VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "invalid-test",
				Namespace: "default",
			},
			Spec: VirtualMachineSpec{
				PowerState:  powerState,
				ProviderRef: ObjectRef{Name: "test-provider"},
				ClassRef:    ObjectRef{Name: "test-class"},
				ImageRef:    ObjectRef{Name: "test-image"},
				Resources: &VirtualMachineResources{
					CPU:       &cpu,
					MemoryMiB: func(m int32) *int64 { v := int64(m); return &v }(memory),
				},
			},
			Status: VirtualMachineStatus{
				Phase: phase,
			},
		}

		// Conversion should not panic even with invalid data
		beta := &v1beta1.VirtualMachine{}
		err := vm.ConvertTo(beta)
		
		// We don't require conversion to succeed with invalid data,
		// but it should not panic
		if err != nil {
			t.Logf("Expected conversion failure with invalid data: %v", err)
			return
		}

		// If conversion succeeded, try round-trip
		roundTrip := &VirtualMachine{}
		err = roundTrip.ConvertFrom(beta)
		if err != nil {
			t.Logf("Expected round-trip failure with invalid data: %v", err)
			return
		}

		// Basic sanity checks - values may be normalized/validated
		t.Logf("Conversion succeeded with potentially invalid data - this is acceptable if values were normalized")
	})
}

