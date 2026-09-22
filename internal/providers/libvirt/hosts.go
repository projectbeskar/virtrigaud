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

package libvirt

import (
	"context"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// ListHosts is the gRPC host-inventory RPC (ADR-0007 P1). It is a
// CLUSTERED-ONLY RPC: in clustered topology it answers with live per-host facts
// gathered from every host the N-host registry fronts; in single-host mode (and
// on an uninitialized server) it stays Unimplemented (D9), which is what
// GetCapabilities.supports_clustering = false promises for those.
//
// The handler delegates to the backend *Provider (which owns the registry and
// the per-host virsh queries) and maps the transport-neutral contract result
// onto the wire HostInfo. A single unreachable host does not fail the call — it
// is reported HOST_HEALTH_NOT_READY while its siblings render (the backend
// enforces this); only a whole-provider failure (e.g. single-host mode) errors.
func (s *Server) ListHosts(ctx context.Context, req *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	if s.provider == nil {
		return nil, errors.NewUnimplemented("ListHosts")
	}
	hosts, err := s.provider.ListHosts(ctx)
	if err != nil {
		// The backend returns a codes.Unimplemented ProviderError in single-host
		// mode and a categorized error otherwise; both carry their own gRPC status.
		return nil, err
	}
	protoHosts := make([]*providerv1.HostInfo, 0, len(hosts))
	for i := range hosts {
		protoHosts = append(protoHosts, hostInfoToProto(hosts[i]))
	}
	return &providerv1.ListHostsResponse{Hosts: protoHosts}, nil
}

// GetHostInfo is the gRPC single-host refresh RPC (ADR-0007 P1) — the cheaper
// counterpart to a full ListHosts poll when reconciling one Host. Like
// ListHosts it is real only in clustered mode and Unimplemented otherwise (D9).
// An unknown host id (not fronted by this provider) returns codes.NotFound;
// a known-but-unreachable host returns a HostInfo with HOST_HEALTH_NOT_READY
// rather than an error, so the caller can record the host as down.
func (s *Server) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	if s.provider == nil {
		return nil, errors.NewUnimplemented("GetHostInfo")
	}
	info, err := s.provider.GetHostInfo(ctx, req.HostId)
	if err != nil {
		return nil, err
	}
	return hostInfoToProto(info), nil
}

// ListHosts returns live inventory for every host the clustered libvirt
// provider fronts (ADR-0007 P1). It is implemented only in CLUSTERED topology;
// in single-host mode it returns a codes.Unimplemented error (D9), matching the
// supports_clustering = false capability the provider advertises there.
//
// It iterates the registry's routable host set (sorted, draining hosts
// excluded) and gathers each host's facts through a briefly-held connection
// lease (collectOneHost, hostinfo.go). One unreachable host is reported
// NotReady and never aborts the whole call — its siblings still render.
func (p *Provider) ListHosts(ctx context.Context) ([]contracts.HostInfo, error) {
	if p.clusterReg == nil {
		return nil, errors.NewUnimplemented("ListHosts")
	}
	ids := p.clusterReg.Hosts()
	hosts := make([]contracts.HostInfo, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, p.collectOneHost(ctx, id))
	}
	return hosts, nil
}

// GetHostInfo returns live inventory for a single host — a cheaper refresh than
// a full ListHosts poll (ADR-0007 P1). Implemented only in CLUSTERED topology
// (Unimplemented otherwise, D9). A host id the registry does not front returns
// a codes.NotFound error; a known-but-unreachable host returns a HostInfo with
// Health = NotReady (not an error), so the caller records it as down.
func (p *Provider) GetHostInfo(ctx context.Context, hostID string) (contracts.HostInfo, error) {
	if p.clusterReg == nil {
		return contracts.HostInfo{}, errors.NewUnimplemented("GetHostInfo")
	}
	id := hostconn.HostID(hostID)
	if _, _, ok := p.clusterReg.HostMeta(id); !ok {
		return contracts.HostInfo{}, errors.NewNotFound("host", hostID)
	}
	return p.collectOneHost(ctx, id), nil
}

// hostInfoToProto maps a manager-side contracts.HostInfo onto the wire
// provider.v1 HostInfo, including the HostHealth enum. It is the inverse of the
// transport client's hostInfoFromProto.
func hostInfoToProto(h contracts.HostInfo) *providerv1.HostInfo {
	return &providerv1.HostInfo{
		Id:                 h.ID,
		Address:            h.Address,
		AllocatableCpu:     h.AllocatableCPU,
		AllocatableMemMib:  h.AllocatableMemMiB,
		AllocatableStorage: h.AllocatableStorage,
		Health:             hostHealthToProto(h.Health),
		Labels:             h.Labels,
		CpuModel:           h.CPUModel,
		CpuFeatures:        h.CPUFeatures,
		MachineTypes:       h.MachineTypes,
		EmulatorVersion:    h.EmulatorVersion,
	}
}

// hostHealthToProto maps the transport-agnostic contracts.HostHealth onto the
// provider.v1 HostHealth enum. An unrecognized/zero value maps to
// HOST_HEALTH_UNSPECIFIED.
func hostHealthToProto(h contracts.HostHealth) providerv1.HostHealth {
	switch h {
	case contracts.HostHealthReady:
		return providerv1.HostHealth_HOST_HEALTH_READY
	case contracts.HostHealthNotReady:
		return providerv1.HostHealth_HOST_HEALTH_NOT_READY
	default:
		return providerv1.HostHealth_HOST_HEALTH_UNSPECIFIED
	}
}
