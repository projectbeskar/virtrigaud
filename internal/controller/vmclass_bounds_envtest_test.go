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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These envtest specs pin the VMClass memory / diskDefaults.size maxima (the
// CRD's core-CEL rules): the API server accepts every plausible quantity, in
// every notation, as an integer or a string, and rejects anything at or above
// the maximum (memory 100Ti, disk 1Pi) or negative. The objects are sent
// unstructured so the integer form reaches the API server as an integer.
var _ = Describe("VMClass memory and disk-size maxima (CRD CEL rules)", func() {
	ctx := context.Background()
	n := 0

	create := func(memory, disk any) error {
		n++
		spec := map[string]any{"cpu": int64(2), "memory": memory}
		if disk != nil {
			spec["diskDefaults"] = map[string]any{"size": disk}
		}
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "infra.virtrigaud.io/v1beta1",
			"kind":       "VMClass",
			"metadata":   map[string]any{"name": fmt.Sprintf("bounds-%d", n), "namespace": "default"},
			"spec":       spec,
		}}
		err := k8sClient.Create(ctx, u)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
		}
		return err
	}
	expectRejected := func(err error, field string) {
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring(field))
	}

	It("accepts every plausible memory quantity", func() {
		for _, m := range []any{
			"512Mi", "4Gi", "4G", "4096Mi", "1.5Ti", "24Ti", "24T", "89Ti", "99Ti", "97000Gi",
			"4294967296", "4e9", "9.8e13", "4000000k", "0", "+8Gi", "0000004Gi",
			int64(4294967296), int64(0),
		} {
			Expect(create(m, nil)).To(Succeed(), "memory %v", m)
		}
	})

	It("rejects memory at or above 100Ti, or negative", func() {
		for _, m := range []any{
			"100Ti", "1Pi", "1Ei", "1P", "102400Gi", "110T", "1e14", "100000000000000", "-4Gi",
			int64(100000000000000), int64(-1),
		} {
			expectRejected(create(m, nil), "memory must be a non-negative quantity below 100Ti")
		}
	})

	It("accepts every plausible disk size and rejects anything at or above 1Pi", func() {
		for _, d := range []any{"40Gi", "2Ti", "62T", "899Ti", "921000Gi", "1e12", int64(1 << 40)} {
			Expect(create("4Gi", d)).To(Succeed(), "disk %v", d)
		}
		for _, d := range []any{"1Pi", "1000Ti", "1024Ti", "1E", "1e15", "-1Gi", int64(1 << 50)} {
			expectRejected(create("4Gi", d), "diskDefaults.size must be a non-negative quantity below 1Pi")
		}
	})

	// For every unit suffix: the largest whole number of that unit below the
	// documented "always accepted" line (memory 90Ti, disk 900Ti) is accepted,
	// and the smallest whole number at or above the maximum (memory 100Ti, disk
	// 1Pi) is rejected.
	It("draws the line at the same magnitude in every unit", func() {
		const ti = int64(1) << 40
		units := map[string]int64{
			"": 1, "k": 1e3, "Ki": 1 << 10, "M": 1e6, "Mi": 1 << 20, "G": 1e9, "Gi": 1 << 30, "T": 1e12, "Ti": ti,
		}
		for suffix, mult := range units {
			memOK := fmt.Sprintf("%d%s", (90*ti-1)/mult, suffix)
			memBad := fmt.Sprintf("%d%s", (100*ti+mult-1)/mult, suffix)
			Expect(create(memOK, nil)).To(Succeed(), "memory %s", memOK)
			expectRejected(create(memBad, nil), "memory must be a non-negative quantity below 100Ti")

			diskOK := fmt.Sprintf("%d%s", (900*ti-1)/mult, suffix)
			diskBad := fmt.Sprintf("%d%s", (1024*ti+mult-1)/mult, suffix)
			Expect(create("4Gi", diskOK)).To(Succeed(), "disk %s", diskOK)
			expectRejected(create("4Gi", diskBad), "diskDefaults.size must be a non-negative quantity below 1Pi")
		}
	})

	It("keeps accepting a VMClass that omits diskDefaults.size (defaulted to 40Gi)", func() {
		Expect(create("4Gi", nil)).To(Succeed())
		Expect(create("4Gi", "999Gi")).To(Succeed())
	})
})
