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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var _ = Describe("VirtualMachineReconciler buildCreateRequest with MetaData", func() {
	var (
		vm         *infravirtrigaudiov1beta1.VirtualMachine
		vmClass    *infravirtrigaudiov1beta1.VMClass
		vmImage    *infravirtrigaudiov1beta1.VMImage
		networks   []*infravirtrigaudiov1beta1.VMNetworkAttachment
		reconciler *VirtualMachineReconciler
	)

	BeforeEach(func() {
		vm = &infravirtrigaudiov1beta1.VirtualMachine{}
		vm.Name = "test-vm"
		vm.Namespace = "default"
		vm.Spec = infravirtrigaudiov1beta1.VirtualMachineSpec{}

		vmClass = &infravirtrigaudiov1beta1.VMClass{}
		vmClass.Name = "small"
		vmClass.Spec = infravirtrigaudiov1beta1.VMClassSpec{
			CPU:    2,
			Memory: resource.MustParse("2Gi"),
		}

		vmImage = &infravirtrigaudiov1beta1.VMImage{}
		vmImage.Name = "ubuntu-22.04"
		vmImage.Spec = infravirtrigaudiov1beta1.VMImageSpec{}

		networks = nil

		reconciler = &VirtualMachineReconciler{}
	})

	Context("when metaData is nil", func() {
		It("should not include metaData in the request", func() {
			vm.Spec.MetaData = nil
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).To(BeNil())
		})
	})

	Context("when metaData is specified with inline YAML", func() {
		It("should include the inline metaData", func() {
			vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline: "instance-id: test-vm-001\nlocal-hostname: test-server",
				},
			}
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).ToNot(BeNil())
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("instance-id: test-vm-001"))
		})
	})

	Context("when metaData contains network configuration", func() {
		It("should preserve the network config in metaData", func() {
			vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline: `instance-id: test-vm-002
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true`,
				},
			}
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).ToNot(BeNil())
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("network:"))
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("version: 2"))
		})
	})

	Context("when both userData and metaData are specified", func() {
		It("should include both in the request", func() {
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
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.UserData).ToNot(BeNil())
			Expect(req.MetaData).ToNot(BeNil())
			Expect(req.UserData.CloudInitData).To(ContainSubstring("packages"))
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("instance-id"))
		})
	})

	Context("when metaData inline is empty string", func() {
		It("should not include metaData", func() {
			vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline: "",
				},
			}
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).To(BeNil())
		})
	})

	Context("when metaData contains public keys", func() {
		It("should include the public keys", func() {
			vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline: `instance-id: test-vm-004
public-keys:
  - ssh-rsa AAAAB3NzaC1yc2E...`,
				},
			}
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).ToNot(BeNil())
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("public-keys"))
		})
	})

	Context("when metaData has complex nested structure", func() {
		It("should preserve the structure", func() {
			vm.Spec.MetaData = &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline: `instance-id: test-vm-005
network:
  version: 2
  ethernets:
    eth0:
      addresses:
        - 192.168.1.100/24
custom:
  tags:
    - production
    - web`,
				},
			}
			req := reconciler.buildCreateRequest(vm, vmClass, vmImage, networks)
			Expect(req.MetaData).ToNot(BeNil())
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("custom:"))
			Expect(req.MetaData.MetaDataYAML).To(ContainSubstring("- production"))
		})
	})
})
