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

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// stubProvider implements contracts.Provider for unit tests.
// Only ReconfigureFn and IsTaskCompleteFn are configurable; all other methods are no-ops.
type stubProvider struct {
	ReconfigureFn    func(ctx context.Context, id string, desired contracts.CreateRequest) (string, error)
	IsTaskCompleteFn func(ctx context.Context, taskRef string) (bool, error)
}

func (s *stubProvider) Validate(_ context.Context) error { return nil }
func (s *stubProvider) Create(_ context.Context, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
	return contracts.CreateResponse{}, nil
}
func (s *stubProvider) Delete(_ context.Context, _ string) (string, error) { return "", nil }
func (s *stubProvider) Power(_ context.Context, _ string, _ contracts.PowerOp) (string, error) {
	return "", nil
}
func (s *stubProvider) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (string, error) {
	if s.ReconfigureFn != nil {
		return s.ReconfigureFn(ctx, id, desired)
	}
	return "", nil
}
func (s *stubProvider) Describe(_ context.Context, _ string) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{}, nil
}
func (s *stubProvider) IsTaskComplete(ctx context.Context, taskRef string) (bool, error) {
	if s.IsTaskCompleteFn != nil {
		return s.IsTaskCompleteFn(ctx, taskRef)
	}
	return true, nil
}
func (s *stubProvider) TaskStatus(_ context.Context, _ string) (contracts.TaskStatus, error) {
	return contracts.TaskStatus{}, nil
}
func (s *stubProvider) SnapshotCreate(_ context.Context, _ contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	return contracts.SnapshotCreateResponse{}, nil
}
func (s *stubProvider) SnapshotDelete(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (s *stubProvider) SnapshotRevert(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (s *stubProvider) ExportDisk(_ context.Context, _ contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	return contracts.ExportDiskResponse{}, nil
}
func (s *stubProvider) ImportDisk(_ context.Context, _ contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	return contracts.ImportDiskResponse{}, nil
}
func (s *stubProvider) GetDiskInfo(_ context.Context, _ contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	return contracts.GetDiskInfoResponse{}, nil
}
func (s *stubProvider) ListVMs(_ context.Context) ([]contracts.VMInfo, error) { return nil, nil }

var _ = Describe("VirtualMachine Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		virtualmachine := &infravirtrigaudiov1beta1.VirtualMachine{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind VirtualMachine")
			err := k8sClient.Get(ctx, typeNamespacedName, virtualmachine)
			if err != nil && errors.IsNotFound(err) {
				resource := &infravirtrigaudiov1beta1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
						ProviderRef: infravirtrigaudiov1beta1.ObjectRef{
							Name: "test-provider",
						},
						ClassRef: infravirtrigaudiov1beta1.ObjectRef{
							Name: "test-class",
						},
						ImageRef: &infravirtrigaudiov1beta1.ObjectRef{
							Name: "test-image",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &infravirtrigaudiov1beta1.VirtualMachine{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VirtualMachine")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &VirtualMachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})

	Context("Resize detection", func() {
		var reconciler *VirtualMachineReconciler

		BeforeEach(func() {
			reconciler = &VirtualMachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		Describe("needsReconfigure", func() {
			It("should return false when CurrentResources is nil (first reconcile)", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				Expect(reconciler.needsReconfigure(vm, vmClass)).To(BeFalse())
			})

			It("should return false when resources match", func() {
				cpu := int32(4)
				memoryMiB := int64(8192)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &cpu,
							MemoryMiB: &memoryMiB,
						},
					},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				Expect(reconciler.needsReconfigure(vm, vmClass)).To(BeFalse())
			})

			It("should return true when CPU changed", func() {
				cpu := int32(2)
				memoryMiB := int64(8192)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &cpu,
							MemoryMiB: &memoryMiB,
						},
					},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				Expect(reconciler.needsReconfigure(vm, vmClass)).To(BeTrue())
			})

			It("should return true when memory changed", func() {
				cpu := int32(4)
				memoryMiB := int64(4096)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &cpu,
							MemoryMiB: &memoryMiB,
						},
					},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				Expect(reconciler.needsReconfigure(vm, vmClass)).To(BeTrue())
			})

			It("should respect VM-level resource overrides", func() {
				cpu := int32(4)
				memoryMiB := int64(8192)
				overrideCPU := int32(8)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
						Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU: &overrideCPU,
						},
					},
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &cpu,
							MemoryMiB: &memoryMiB,
						},
					},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				// VM override sets CPU to 8, current is 4, so should need reconfigure
				Expect(reconciler.needsReconfigure(vm, vmClass)).To(BeTrue())
			})
		})

		Describe("getCurrentCPU", func() {
			It("should return 0 when CurrentResources is nil", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{},
				}
				Expect(reconciler.getCurrentCPU(vm)).To(Equal(int32(0)))
			})

			It("should return CPU value when set", func() {
				cpu := int32(4)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU: &cpu,
						},
					},
				}
				Expect(reconciler.getCurrentCPU(vm)).To(Equal(int32(4)))
			})
		})

		Describe("getCurrentMemoryMiB", func() {
			It("should return 0 when CurrentResources is nil", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{},
				}
				Expect(reconciler.getCurrentMemoryMiB(vm)).To(Equal(int64(0)))
			})

			It("should return memory value when set", func() {
				memoryMiB := int64(8192)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							MemoryMiB: &memoryMiB,
						},
					},
				}
				Expect(reconciler.getCurrentMemoryMiB(vm)).To(Equal(int64(8192)))
			})
		})

		Describe("reconfigureVM", func() {
			var vm *infravirtrigaudiov1beta1.VirtualMachine
			var vmClass *infravirtrigaudiov1beta1.VMClass

			BeforeEach(func() {
				vm = &infravirtrigaudiov1beta1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "reconfig-test",
						Namespace: "default",
					},
					Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
						ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-provider"},
						ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "test-class"},
					},
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						ID: "vm-123",
					},
				}
				vmClass = &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}
			})

			It("should set error condition and requeue when provider returns error", func() {
				provider := &stubProvider{
					ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
						return "", fmt.Errorf("provider unavailable")
					},
				}

				result, err := reconciler.reconfigureVM(ctx, vm, provider, vmClass, nil, nil)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Second))
				found := false
				for _, c := range vm.Status.Conditions {
					if c.Type == "Reconfiguring" {
						found = true
						Expect(string(c.Status)).To(Equal("False"))
						Expect(c.Reason).To(Equal("ProviderError"))
					}
				}
				Expect(found).To(BeTrue())
			})

			It("should set ReconfigureTaskRef and phase Reconfiguring for async operation", func() {
				provider := &stubProvider{
					ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
						return "task-456", nil
					},
				}

				result, err := reconciler.reconfigureVM(ctx, vm, provider, vmClass, nil, nil)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Second))
				Expect(vm.Status.ReconfigureTaskRef).To(Equal("task-456"))
				Expect(vm.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring))
				Expect(vm.Status.LastReconfigureTime).NotTo(BeNil())
			})

			It("should update CurrentResources and set phase Running for synchronous completion", func() {
				provider := &stubProvider{
					ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
						return "", nil
					},
				}

				result, err := reconciler.reconfigureVM(ctx, vm, provider, vmClass, nil, nil)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(5 * time.Second))
				Expect(vm.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseRunning))
				Expect(vm.Status.CurrentResources).NotTo(BeNil())
				Expect(*vm.Status.CurrentResources.CPU).To(Equal(int32(4)))
				Expect(*vm.Status.CurrentResources.MemoryMiB).To(Equal(int64(8192)))
			})
		})

		Describe("getRequeueInterval", func() {
			It("should return 10s for poweredOn VM with no IPs (waiting for VMware Tools)", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				desc := contracts.DescribeResponse{PowerState: "poweredOn", IPs: nil}
				Expect(reconciler.getRequeueInterval(vm, desc)).To(Equal(10 * time.Second))
			})

			It("should return 2m for poweredOn VM with IPs", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				desc := contracts.DescribeResponse{PowerState: "poweredOn", IPs: []string{"10.0.0.1"}}
				Expect(reconciler.getRequeueInterval(vm, desc)).To(Equal(2 * time.Minute))
			})

			It("should return 5m for poweredOff VM", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				desc := contracts.DescribeResponse{PowerState: "poweredOff"}
				Expect(reconciler.getRequeueInterval(vm, desc)).To(Equal(5 * time.Minute))
			})

			It("should return 2m for suspended VM", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				desc := contracts.DescribeResponse{PowerState: "suspended"}
				Expect(reconciler.getRequeueInterval(vm, desc)).To(Equal(2 * time.Minute))
			})

			It("should return 10s for unknown/transitional state", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				desc := contracts.DescribeResponse{PowerState: "unknown"}
				Expect(reconciler.getRequeueInterval(vm, desc)).To(Equal(10 * time.Second))
			})
		})

		Describe("updateCurrentResources", func() {
			It("should initialize CurrentResources when nil", func() {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				reconciler.updateCurrentResources(vm, vmClass)

				Expect(vm.Status.CurrentResources).NotTo(BeNil())
				Expect(*vm.Status.CurrentResources.CPU).To(Equal(int32(4)))
				Expect(*vm.Status.CurrentResources.MemoryMiB).To(Equal(int64(8192)))
			})

			It("should update existing CurrentResources", func() {
				oldCPU := int32(2)
				oldMemory := int64(4096)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
						CurrentResources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &oldCPU,
							MemoryMiB: &oldMemory,
						},
					},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    8,
						Memory: resource.MustParse("16Gi"),
					},
				}

				reconciler.updateCurrentResources(vm, vmClass)

				Expect(*vm.Status.CurrentResources.CPU).To(Equal(int32(8)))
				Expect(*vm.Status.CurrentResources.MemoryMiB).To(Equal(int64(16384)))
			})

			It("should respect VM-level resource overrides", func() {
				overrideCPU := int32(16)
				overrideMemory := int64(32768)
				vm := &infravirtrigaudiov1beta1.VirtualMachine{
					Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
						Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{
							CPU:       &overrideCPU,
							MemoryMiB: &overrideMemory,
						},
					},
					Status: infravirtrigaudiov1beta1.VirtualMachineStatus{},
				}
				vmClass := &infravirtrigaudiov1beta1.VMClass{
					Spec: infravirtrigaudiov1beta1.VMClassSpec{
						CPU:    4,
						Memory: resource.MustParse("8Gi"),
					},
				}

				reconciler.updateCurrentResources(vm, vmClass)

				// Should use VM overrides, not VMClass values
				Expect(*vm.Status.CurrentResources.CPU).To(Equal(int32(16)))
				Expect(*vm.Status.CurrentResources.MemoryMiB).To(Equal(int64(32768)))
			})
		})
	})
})
