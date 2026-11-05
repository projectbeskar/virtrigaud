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

package clustered

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestHostInfoToProto(t *testing.T) {
	got := HostInfoToProto(contracts.HostInfo{
		ID: "host-a", Address: "10.0.0.1", AllocatableCPU: 8, AllocatableMemMiB: 16384,
		AllocatableStorage: 1 << 40, Health: contracts.HostHealthReady,
		Labels: map[string]string{"rack": "1"}, CPUModel: "EPYC", CPUFeatures: []string{"avx2"},
		MachineTypes: []string{"q35"}, EmulatorVersion: "8.2.0",
	})
	assert.Equal(t, &providerv1.HostInfo{
		Id: "host-a", Address: "10.0.0.1", AllocatableCpu: 8, AllocatableMemMib: 16384,
		AllocatableStorage: 1 << 40, Health: providerv1.HostHealth_HOST_HEALTH_READY,
		Labels: map[string]string{"rack": "1"}, CpuModel: "EPYC", CpuFeatures: []string{"avx2"},
		MachineTypes: []string{"q35"}, EmulatorVersion: "8.2.0",
	}, got)
}

func TestHostHealthToProto(t *testing.T) {
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_READY, HostHealthToProto(contracts.HostHealthReady))
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_NOT_READY, HostHealthToProto(contracts.HostHealthNotReady))
	assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_UNSPECIFIED, HostHealthToProto(contracts.HostHealth("")))
}
