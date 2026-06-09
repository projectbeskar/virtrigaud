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

package controller

import (
	"context"
	"strings"
	"testing"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newMetaTestFixtures returns a default VM, VMClass, and VMImage for inline metadata tests.
// These tests do not require a live k8s cluster — the reconciler has no client.
func newMetaTestFixtures() (*infravirtrigaudiov1beta1.VirtualMachine, *infravirtrigaudiov1beta1.VMClass, *infravirtrigaudiov1beta1.VMImage) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vm", Namespace: "default"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{Name: "small"},
		Spec:       infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("2Gi")},
	}
	vmImage := &infravirtrigaudiov1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-22.04"},
		Spec:       infravirtrigaudiov1beta1.VMImageSpec{},
	}
	return vm, vmClass, vmImage
}

func TestBuildCreateRequest_Metadata_NilMetaData_NoMetaDataInRequest(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = nil
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData != nil {
		t.Error("expected MetaData to be nil when spec.MetaData is nil")
	}
}

func TestBuildCreateRequest_Metadata_InlineYAML_IncludedInRequest(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "instance-id: test-vm-001\nlocal-hostname: test-server",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "instance-id: test-vm-001") {
		t.Errorf("expected inline content in MetaData, got: %q", req.MetaData.MetaDataYAML)
	}
}

func TestBuildCreateRequest_Metadata_NetworkConfig_Preserved(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "instance-id: test-vm-002\nnetwork:\n  version: 2\n  ethernets:\n    eth0:\n      dhcp4: true",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "network:") {
		t.Error("expected network config in MetaData")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "version: 2") {
		t.Error("expected version: 2 in MetaData")
	}
}

func TestBuildCreateRequest_BothUserDataAndMetaData_BothIncluded(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.UserData = &infravirtrigaudiov1beta1.UserData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInit{
			Inline: "#cloud-config\npackages:\n  - nginx",
		},
	}
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "instance-id: test-vm-003",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.UserData == nil {
		t.Error("expected UserData to be set")
	}
	if req.MetaData == nil {
		t.Error("expected MetaData to be set")
	}
	if !strings.Contains(req.UserData.CloudInitData, "packages") {
		t.Errorf("expected packages in UserData, got: %q", req.UserData.CloudInitData)
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "instance-id") {
		t.Errorf("expected instance-id in MetaData, got: %q", req.MetaData.MetaDataYAML)
	}
}

func TestBuildCreateRequest_Metadata_EmptyInline_NoMetaData(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData != nil {
		t.Errorf("expected MetaData to be nil when inline is empty, got: %+v", req.MetaData)
	}
}

func TestBuildCreateRequest_Metadata_PublicKeys_Included(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "instance-id: test-vm-004\npublic-keys:\n  - ssh-rsa AAAAB3NzaC1yc2E...",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "public-keys") {
		t.Errorf("expected public-keys in MetaData, got: %q", req.MetaData.MetaDataYAML)
	}
}

func TestBuildCreateRequest_Metadata_ComplexNestedStructure_Preserved(t *testing.T) {
	vm, vmClass, vmImage := newMetaTestFixtures()
	vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
		CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
			Inline: "instance-id: test-vm-005\ncustom:\n  tags:\n    - production\n    - web",
		},
	}
	r := &VirtualMachineReconciler{}

	req, err := r.buildCreateRequest(context.Background(), vm, "", vmClass, vmImage, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "custom:") {
		t.Error("expected custom: in MetaData")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "- production") {
		t.Error("expected - production in MetaData")
	}
}
