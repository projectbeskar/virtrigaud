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
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// HostInfoToProto maps a manager-side contracts.HostInfo onto the wire
// provider.v1 HostInfo of the ListHosts and GetHostInfo RPCs (ADR-0007 P1),
// including the HostHealth enum. It is the inverse of the transport client's
// hostInfoFromProto.
func HostInfoToProto(h contracts.HostInfo) *providerv1.HostInfo {
	return &providerv1.HostInfo{
		Id:                 h.ID,
		Address:            h.Address,
		AllocatableCpu:     h.AllocatableCPU,
		AllocatableMemMib:  h.AllocatableMemMiB,
		AllocatableStorage: h.AllocatableStorage,
		Health:             HostHealthToProto(h.Health),
		Labels:             h.Labels,
		CpuModel:           h.CPUModel,
		CpuFeatures:        h.CPUFeatures,
		MachineTypes:       h.MachineTypes,
		EmulatorVersion:    h.EmulatorVersion,
	}
}

// HostHealthToProto maps the transport-agnostic contracts.HostHealth onto the
// provider.v1 HostHealth enum. An unrecognized/zero value maps to
// HOST_HEALTH_UNSPECIFIED.
func HostHealthToProto(h contracts.HostHealth) providerv1.HostHealth {
	switch h {
	case contracts.HostHealthReady:
		return providerv1.HostHealth_HOST_HEALTH_READY
	case contracts.HostHealthNotReady:
		return providerv1.HostHealth_HOST_HEALTH_NOT_READY
	default:
		return providerv1.HostHealth_HOST_HEALTH_UNSPECIFIED
	}
}
