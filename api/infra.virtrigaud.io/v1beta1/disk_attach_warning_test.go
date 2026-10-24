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

package v1beta1

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestVirtualMachineWebhook_UnattachedDisksWarning pins V4's admission side:
// a VirtualMachine with spec.disks on a Provider that does not attach them
// (libvirt, Proxmox) is admitted with a warning; on vSphere, without
// spec.disks, with the Provider missing or in another namespace, or without a
// reader, there is no warning — and admission never fails because of it.
func TestVirtualMachineWebhook_UnattachedDisksWarning(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	provider := func(name string, typ ProviderType) *Provider {
		return &Provider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}, Spec: ProviderSpec{Type: typ}}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		provider("kvm", ProviderTypeLibvirt), provider("pve", ProviderTypeProxmox), provider("vc", ProviderTypeVSphere)).Build()
	vm := func(providerName, providerNS string, disks ...DiskSpec) *VirtualMachine {
		return &VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
			Spec:       VirtualMachineSpec{ProviderRef: ObjectRef{Name: providerName, Namespace: providerNS}, Disks: disks},
		}
	}
	data := DiskSpec{Name: "data", SizeGiB: 1}
	ceiling := resource.MustParse("100Gi")

	for _, tc := range []struct {
		name   string
		vm     *VirtualMachine
		reader bool
		warn   bool
	}{
		{"libvirt with spec.disks", vm("kvm", "", data), true, true},
		{"proxmox with spec.disks", vm("pve", "", data), true, true},
		{"libvirt, provider in the VM's namespace named explicitly", vm("kvm", "team-a", data), true, true},
		{"vsphere attaches them", vm("vc", "", data), true, false},
		{"libvirt without spec.disks", vm("kvm", ""), true, false},
		{"provider missing", vm("absent", "", data), true, false},
		{"provider in another namespace is not read", vm("kvm", "team-b", data), true, false},
		{"no reader", vm("kvm", "", data), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &VirtualMachineDiskSizeValidator{MaxDiskSize: &ceiling}
			if tc.reader {
				v.Reader = reader
			}
			ctx := context.Background()
			created, err := v.ValidateCreate(ctx, tc.vm)
			if err != nil {
				t.Fatalf("admission must not fail: %v", err)
			}
			updated, err := v.ValidateUpdate(ctx, vm("kvm", ""), tc.vm)
			if err != nil {
				t.Fatalf("admission must not fail: %v", err)
			}
			for _, warnings := range [][]string{created, updated} {
				if !tc.warn {
					if len(warnings) != 0 {
						t.Fatalf("want no warning, got %v", warnings)
					}
					continue
				}
				if len(warnings) != 1 || !strings.Contains(warnings[0], "does not attach additional disks") ||
					!strings.Contains(warnings[0], "DisksApplied") {
					t.Fatalf("want the spec.disks warning, got %v", warnings)
				}
			}
		})
	}

	// A disk above the ceiling is still refused, with the warning alongside.
	v := &VirtualMachineDiskSizeValidator{MaxDiskSize: &ceiling, Reader: reader}
	warnings, err := v.ValidateCreate(context.Background(), vm("kvm", "", DiskSpec{Name: "big", SizeGiB: 500}))
	if err == nil {
		t.Fatal("a disk above the ceiling is refused")
	}
	if len(warnings) != 1 {
		t.Fatalf("want the warning with the refusal, got %v", warnings)
	}
}

func TestProviderIgnoresAdditionalDisks(t *testing.T) {
	for typ, want := range map[ProviderType]bool{
		ProviderTypeLibvirt: true, ProviderTypeProxmox: true, ProviderTypeVSphere: false,
		ProviderTypeFirecracker: false, ProviderTypeQEMU: false,
	} {
		if got := ProviderIgnoresAdditionalDisks(typ); got != want {
			t.Errorf("%s: got %v, want %v", typ, got, want)
		}
	}
}
