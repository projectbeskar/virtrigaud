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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the overflow fix for VMClass memory / diskDefaults.size: the
// int32 MiB / GiB the provider contract carries is range-checked first, and an
// out-of-range value is an InvalidSpec error — never a wrapped-around size.

func TestVMClassQuantityUnits(t *testing.T) {
	cases := []struct {
		name    string
		q       string
		unit    int64
		max     int64
		want    int32
		wantErr bool
	}{
		{"memory 4Gi", "4Gi", bytesPerMiB, maxVMClassMemoryBytes, 4096, false},
		{"memory just below 100Ti", "102399Gi", bytesPerMiB, maxVMClassMemoryBytes, 104856576, false},
		{"memory 100Ti is the exclusive maximum", "100Ti", bytesPerMiB, maxVMClassMemoryBytes, 0, true},
		// 2Pi of memory is 2^31 MiB: the old int32 conversion wrapped it to a
		// negative size.
		{"memory that used to wrap int32", "2Pi", bytesPerMiB, maxVMClassMemoryBytes, 0, true},
		{"memory beyond int64", "100Ei", bytesPerMiB, maxVMClassMemoryBytes, 0, true},
		{"negative memory", "-1Gi", bytesPerMiB, maxVMClassMemoryBytes, 0, true},
		{"zero memory", "0", bytesPerMiB, maxVMClassMemoryBytes, 0, false},
		{"disk 40Gi", "40Gi", bytesPerGiB, maxVMClassDiskBytes, 40, false},
		{"disk just below 1Pi", "1023Ti", bytesPerGiB, maxVMClassDiskBytes, 1047552, false},
		{"disk 1Pi is the exclusive maximum", "1Pi", bytesPerGiB, maxVMClassDiskBytes, 0, true},
		{"disk that used to wrap int32", "2Ei", bytesPerGiB, maxVMClassDiskBytes, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vmClassQuantityUnits("field", resource.MustParse(tc.q), tc.unit, tc.max)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCreateVM_OutOfRangeVMClassIsAValidationError: a VMClass stored before
// the CRD maxima (or on a path that bypassed them) cannot reach a provider
// with a wrapped size: Create is never called, and the VM shows
// Provisioning=False/ValidationError on the spec cadence.
func TestCreateVM_OutOfRangeVMClassIsAValidationError(t *testing.T) {
	for name, mutate := range map[string]func(*infravirtrigaudiov1beta1.VMClass){
		"memory": func(c *infravirtrigaudiov1beta1.VMClass) { c.Spec.Memory = resource.MustParse("2Pi") },
		"disk": func(c *infravirtrigaudiov1beta1.VMClass) {
			c.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse("4Ei")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("big", clusteredNS, "prov-single")
			class := smallVMClass(clusteredNS)
			mutate(class)
			prov := &routingProvider{}
			providerCR := withRuntime(singleProviderCR("prov-single", clusteredNS))
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR, class, minimalVMImage(clusteredNS))

			res, err := r.createVM(context.Background(), getVM(t, r, "big"), prov, providerCR, class, minimalVMImage(clusteredNS), nil)
			require.NoError(t, err)
			assert.Empty(t, prov.createReqs, "no provider call with an out-of-range size")
			assert.Equal(t, vmCreateInvalidSpecRetryInterval, res.RequeueAfter)
			assert.Equal(t, k8s.ReasonValidationError, provisioningReason(getVM(t, r, "big")))
		})
	}
}
