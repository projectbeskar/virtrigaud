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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

var _ = Describe("VirtualMachineReconciler lifecycle", func() {
	var (
		vm         *infrav1beta1.VirtualMachine
		vmClass    *infrav1beta1.VMClass
		reconciler *VirtualMachineReconciler
	)

	BeforeEach(func() {
		vm = &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-vm",
				Namespace: "default",
				UID:       "test-uid-1234",
			},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "my-provider"},
				ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
				Placement: &infrav1beta1.Placement{
					Cluster:   "dc0/cls0",
					Datastore: "datastore1",
				},
			},
		}

		vmClass = &infrav1beta1.VMClass{
			ObjectMeta: metav1.ObjectMeta{Name: "small"},
			Spec: infrav1beta1.VMClassSpec{
				CPU:    4,
				Memory: resource.MustParse("8Gi"),
			},
		}

		reconciler = &VirtualMachineReconciler{}
	})

	// -------------------------------------------------------------------------
	// buildLifecycleJob
	// -------------------------------------------------------------------------
	Describe("buildLifecycleJob", func() {
		var action *infrav1beta1.JobAction

		BeforeEach(func() {
			action = &infrav1beta1.JobAction{
				Image: "registry.example.com/hook:latest",
			}
		})

		It("sets the correct env vars from VM and VMClass", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			envMap := envVarMap(job)
			Expect(envMap["VM_NAME"]).To(Equal("test-vm"))
			Expect(envMap["VM_NAMESPACE"]).To(Equal("default"))
			Expect(envMap["VM_CLASS"]).To(Equal("small"))
			Expect(envMap["VM_CLUSTER"]).To(Equal("dc0/cls0"))
			Expect(envMap["VM_DATASTORE"]).To(Equal("datastore1"))
			Expect(envMap["VM_CPU"]).To(Equal("4"))
			Expect(envMap["VM_MEMORY"]).To(Equal("8Gi"))
			Expect(envMap["LIFECYCLE_EVENT"]).To(Equal("preStart"))
		})

		It("falls back to StoragePod when Datastore is empty", func() {
			vm.Spec.Placement = &infrav1beta1.Placement{
				Cluster:    "dc0/cls0",
				StoragePod: "ds-cluster-1",
			}
			job := reconciler.buildLifecycleJob(vm, vmClass, "postStop", action)

			envMap := envVarMap(job)
			Expect(envMap["VM_DATASTORE"]).To(Equal("ds-cluster-1"))
		})

		It("sets cluster and datastore to empty strings when Placement is nil", func() {
			vm.Spec.Placement = nil
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			envMap := envVarMap(job)
			Expect(envMap["VM_CLUSTER"]).To(Equal(""))
			Expect(envMap["VM_DATASTORE"]).To(Equal(""))
		})

		It("defaults ImagePullPolicy to IfNotPresent when not set", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.Template.Spec.Containers[0].ImagePullPolicy).
				To(Equal(corev1.PullIfNotPresent))
		})

		It("respects a custom ImagePullPolicy", func() {
			action.ImagePullPolicy = "Always"
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.Template.Spec.Containers[0].ImagePullPolicy).
				To(Equal(corev1.PullAlways))
		})

		It("converts ImagePullSecrets to corev1.LocalObjectReference", func() {
			action.ImagePullSecrets = []infrav1beta1.LocalObjectReference{
				{Name: "secret-one"},
				{Name: "secret-two"},
			}
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			secrets := job.Spec.Template.Spec.ImagePullSecrets
			Expect(secrets).To(HaveLen(2))
			Expect(secrets[0].Name).To(Equal("secret-one"))
			Expect(secrets[1].Name).To(Equal("secret-two"))
		})

		It("sets an empty ImagePullSecrets slice when none are provided", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)
			Expect(job.Spec.Template.Spec.ImagePullSecrets).To(BeEmpty())
		})

		It("sets ServiceAccountName when provided", func() {
			action.ServiceAccountName = "hook-sa"
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("hook-sa"))
		})

		It("defaults BackoffLimit to 3 when not set", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(3)))
		})

		It("uses a custom BackoffLimit when set", func() {
			limit := int32(1)
			action.BackoffLimit = &limit
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(*job.Spec.BackoffLimit).To(Equal(int32(1)))
		})

		It("sets ActiveDeadlineSeconds when provided", func() {
			deadline := int64(120)
			action.ActiveDeadlineSeconds = &deadline
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.ActiveDeadlineSeconds).NotTo(BeNil())
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(120)))
		})

		It("leaves ActiveDeadlineSeconds nil when not provided", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)
			Expect(job.Spec.ActiveDeadlineSeconds).To(BeNil())
		})

		It("sets RestartPolicy to Never", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
		})

		It("sets the container image correctly", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Spec.Template.Spec.Containers[0].Image).
				To(Equal("registry.example.com/hook:latest"))
		})

		It("labels the Job with the VM name and lifecycle event", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "postStop", action)

			Expect(job.Labels["infra.virtrigaud.io/vm"]).To(Equal("test-vm"))
			Expect(job.Labels["infra.virtrigaud.io/lifecycle-event"]).To(Equal("postStop"))
		})

		It("places the Job in the VM namespace", func() {
			vm.Namespace = "production"
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(job.Namespace).To(Equal("production"))
		})

		It("generates a job name containing the event slug", func() {
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)
			Expect(job.Name).To(ContainSubstring("prestart"))
		})

		It("truncates the job name to 63 characters for very long VM names", func() {
			vm.Name = strings.Repeat("a", 60)
			job := reconciler.buildLifecycleJob(vm, vmClass, "preStart", action)

			Expect(len(job.Name)).To(BeNumerically("<=", 63))
		})
	})

	// -------------------------------------------------------------------------
	// checkLifecycleJob
	// -------------------------------------------------------------------------
	Describe("checkLifecycleJob", func() {
		var (
			ctx        context.Context
			fakeClient client.Client
		)

		BeforeEach(func() {
			ctx = context.Background()

			s := scheme.Scheme
			Expect(infrav1beta1.AddToScheme(s)).To(Succeed())
			Expect(batchv1.AddToScheme(s)).To(Succeed())

			fakeClient = fake.NewClientBuilder().WithScheme(s).Build()
			reconciler = &VirtualMachineReconciler{Client: fakeClient, Scheme: s}

			vm.Status.LifecycleJobRef = "test-vm-prestart-abc123"
		})

		It("returns done=true when the Job does not exist", func() {
			done, failed, err := reconciler.checkLifecycleJob(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(failed).To(BeFalse())
		})

		It("returns done=true, no error when the Job has completed successfully", func() {
			job := jobFixture(vm.Status.LifecycleJobRef, vm.Namespace, batchv1.JobComplete)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			done, failed, err := reconciler.checkLifecycleJob(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(failed).To(BeFalse())
		})

		It("returns done=true and an error when the Job has failed", func() {
			job := jobFixture(vm.Status.LifecycleJobRef, vm.Namespace, batchv1.JobFailed)
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			done, failed, err := reconciler.checkLifecycleJob(ctx, vm)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("lifecycle job"))
			Expect(err.Error()).To(ContainSubstring("failed"))
			Expect(done).To(BeTrue())
			Expect(failed).To(BeTrue())
		})

		It("returns done=false when the Job is still running", func() {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vm.Status.LifecycleJobRef,
					Namespace: vm.Namespace,
				},
			}
			Expect(fakeClient.Create(ctx, job)).To(Succeed())

			done, failed, err := reconciler.checkLifecycleJob(ctx, vm)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(failed).To(BeFalse())
		})
	})

	// -------------------------------------------------------------------------
	// startLifecycleJob
	// -------------------------------------------------------------------------
	Describe("startLifecycleJob", func() {
		var (
			ctx        context.Context
			fakeClient client.Client
			action     *infrav1beta1.JobAction
		)

		BeforeEach(func() {
			ctx = context.Background()

			s := scheme.Scheme
			Expect(infrav1beta1.AddToScheme(s)).To(Succeed())
			Expect(batchv1.AddToScheme(s)).To(Succeed())

			fakeClient = fake.NewClientBuilder().
				WithScheme(s).
				WithStatusSubresource(&infrav1beta1.VirtualMachine{}).
				Build()
			reconciler = &VirtualMachineReconciler{Client: fakeClient, Scheme: s}

			action = &infrav1beta1.JobAction{
				Image: "registry.example.com/hook:latest",
			}
		})

		It("creates the Job in the cluster", func() {
			// Create the VM so SetControllerReference can look up GVK
			Expect(fakeClient.Create(ctx, vm)).To(Succeed())

			result, err := reconciler.startLifecycleJob(ctx, vm, vmClass, "preStart", action)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero())

			// Verify the Job exists
			jobList := &batchv1.JobList{}
			Expect(fakeClient.List(ctx, jobList, client.InNamespace("default"))).To(Succeed())
			Expect(jobList.Items).To(HaveLen(1))
		})

		It("records the Job name in LifecycleJobRef", func() {
			Expect(fakeClient.Create(ctx, vm)).To(Succeed())

			_, err := reconciler.startLifecycleJob(ctx, vm, vmClass, "preStart", action)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm.Status.LifecycleJobRef).NotTo(BeEmpty())
		})

		It("records the event in LifecyclePhase", func() {
			Expect(fakeClient.Create(ctx, vm)).To(Succeed())

			_, err := reconciler.startLifecycleJob(ctx, vm, vmClass, "postStop", action)
			Expect(err).NotTo(HaveOccurred())

			Expect(vm.Status.LifecyclePhase).To(Equal("postStop"))
		})

		It("sets an owner reference on the Job pointing to the VM", func() {
			Expect(fakeClient.Create(ctx, vm)).To(Succeed())

			_, err := reconciler.startLifecycleJob(ctx, vm, vmClass, "preStart", action)
			Expect(err).NotTo(HaveOccurred())

			jobList := &batchv1.JobList{}
			Expect(fakeClient.List(ctx, jobList, client.InNamespace("default"))).To(Succeed())
			Expect(jobList.Items).To(HaveLen(1))

			job := jobList.Items[0]
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Name).To(Equal(vm.Name))
		})

		It("persists the updated status to the fake client", func() {
			Expect(fakeClient.Create(ctx, vm)).To(Succeed())

			_, err := reconciler.startLifecycleJob(ctx, vm, vmClass, "preStart", action)
			Expect(err).NotTo(HaveOccurred())

			updated := &infrav1beta1.VirtualMachine{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Name: vm.Name, Namespace: vm.Namespace}, updated)).To(Succeed())
			Expect(updated.Status.LifecycleJobRef).NotTo(BeEmpty())
			Expect(updated.Status.LifecyclePhase).To(Equal("preStart"))
		})
	})
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// envVarMap converts the env var slice on the first container into a map.
func envVarMap(job *batchv1.Job) map[string]string {
	m := make(map[string]string)
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		m[e.Name] = e.Value
	}
	return m
}

// jobFixture builds a minimal batchv1.Job with the given condition type set.
func jobFixture(name, namespace string, condType batchv1.JobConditionType) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:   condType,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}
