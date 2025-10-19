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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
)

var _ = Describe("VMMigration Controller", func() {
	var (
		ctx        context.Context
		reconciler *VMMigrationReconciler
		fakeClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Register our scheme
		s := scheme.Scheme
		err := infrav1beta1.AddToScheme(s)
		Expect(err).NotTo(HaveOccurred())

		// Create fake client
		fakeClient = fake.NewClientBuilder().
			WithScheme(s).
			Build()

		// Create reconciler with fake client
		reconciler = &VMMigrationReconciler{
			Client:         fakeClient,
			Scheme:         s,
			RemoteResolver: remote.NewResolver(fakeClient),
		}
	})

	Describe("Reconcile", func() {
		Context("when VMMigration doesn't exist", func() {
			It("should return without error", func() {
				req := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      "nonexistent-migration",
						Namespace: "default",
					},
				}

				result, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())
			})
		})

		Context("when VMMigration is created", func() {
			It("should initialize with Pending phase", func() {
				migration := &infrav1beta1.VMMigration{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-migration",
						Namespace: "default",
					},
					Spec: infrav1beta1.VMMigrationSpec{
						Source: infrav1beta1.MigrationSource{
							VMRef: infrav1beta1.LocalObjectReference{
								Name: "source-vm",
							},
						},
						Target: infrav1beta1.MigrationTarget{
							Name: "target-vm",
							ProviderRef: infrav1beta1.ObjectRef{
								Name: "target-provider",
							},
						},
					},
				}

				err := fakeClient.Create(ctx, migration)
				Expect(err).NotTo(HaveOccurred())

				req := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      migration.Name,
						Namespace: migration.Namespace,
					},
				}

				result, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeTrue())

				// Verify migration was updated with finalizer
				updated := &infrav1beta1.VMMigration{}
				err = fakeClient.Get(ctx, req.NamespacedName, updated)
				Expect(err).NotTo(HaveOccurred())
				Expect(updated.Finalizers).To(ContainElement("vmmigration.infra.virtrigaud.io/finalizer"))
			})
		})

		Context("when VMMigration is being deleted", func() {
			It("should handle deletion properly", func() {
				migration := &infrav1beta1.VMMigration{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-migration",
						Namespace:  "default",
						Finalizers: []string{"vmmigration.infra.virtrigaud.io/finalizer"},
					},
					Spec: infrav1beta1.VMMigrationSpec{
						Source: infrav1beta1.MigrationSource{
							VMRef: infrav1beta1.LocalObjectReference{
								Name: "source-vm",
							},
						},
						Target: infrav1beta1.MigrationTarget{
							Name: "target-vm",
							ProviderRef: infrav1beta1.ObjectRef{
								Name: "target-provider",
							},
						},
					},
				}

				err := fakeClient.Create(ctx, migration)
				Expect(err).NotTo(HaveOccurred())

				// First reconcile adds finalizer if needed
				req := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      migration.Name,
						Namespace: migration.Namespace,
					},
				}

				// Set deletion timestamp
				err = fakeClient.Delete(ctx, migration)
				Expect(err).NotTo(HaveOccurred())

				// Reconcile should handle deletion
				result, err := reconciler.Reconcile(ctx, req)
				// The resource is deleted, so we expect "not found" which reconcile handles gracefully
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())
			})
		})
	})

	Describe("generateStorageURL", func() {
		var migration *infrav1beta1.VMMigration

		BeforeEach(func() {
			migration = &infrav1beta1.VMMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "default",
				},
			}
		})

		Context("with PVC storage", func() {
			It("should generate correct PVC URL", func() {
				migration.Spec.Storage = &infrav1beta1.MigrationStorage{
					Type: "pvc",
					PVC: &infrav1beta1.PVCStorageConfig{
						Name: "test-pvc",
					},
				}

				url, err := reconciler.generateStorageURL(ctx, migration, "export")
				Expect(err).NotTo(HaveOccurred())
				Expect(url).To(Equal("pvc://vmmigrations/default/test-migration/export.qcow2"))
			})

			It("should default to PVC when type is empty", func() {
				migration.Spec.Storage = &infrav1beta1.MigrationStorage{
					Type: "",
					PVC: &infrav1beta1.PVCStorageConfig{
						Name: "test-pvc",
					},
				}

				url, err := reconciler.generateStorageURL(ctx, migration, "export")
				Expect(err).NotTo(HaveOccurred())
				Expect(url).To(Equal("pvc://vmmigrations/default/test-migration/export.qcow2"))
			})
		})

		Context("with no storage configured", func() {
			It("should return error", func() {
				migration.Spec.Storage = nil

				_, err := reconciler.generateStorageURL(ctx, migration, "export")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("storage configuration is required"))
			})
		})

		Context("with unsupported storage type", func() {
			It("should return error", func() {
				migration.Spec.Storage = &infrav1beta1.MigrationStorage{
					Type: "unsupported",
				}

				_, err := reconciler.generateStorageURL(ctx, migration, "export")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("unsupported storage type"))
			})
		})
	})
})
