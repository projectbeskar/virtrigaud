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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// Power states the fake reports (the contracts values the libvirt provider
// reports).
const (
	powerOn  = string(contracts.PowerStateOn)
	powerOff = string(contracts.PowerStateOff)
)

// fakeServer implements the provider contract for a clustered provider
// whose hosts are run by (simulated) per-host agents. Each RPC sleeps its
// configured latency; operations that change a host also wait for one of the
// host's execution slots, as an agent with bounded concurrency would.
//
// It follows the clustered contract the libvirt provider implements
// (ADR-0007 Addendum A): every per-VM call names its host
// (target_host_id), never defaults one; the VM's id is "<namespace>.<name>";
// a VM belongs to the owner UID stamped at create; a repeated Create of the
// same owner on the same host binds the existing VM; a Create whose name is
// taken by another owner answers AlreadyExists; a Describe or Delete by
// another owner sees nothing.
type fakeServer struct {
	providerv1.UnimplementedProviderServer
	st *state
	m  *fakeMetrics
}

// observe sleeps the method's latency and records the call.
func (f *fakeServer) observe(method string, start time.Time, err *error) {
	code := status.Code(*err).String()
	f.m.rpcTotal.WithLabelValues(method, code).Inc()
	f.m.rpcDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

// delay sleeps the configured latency of method, or until ctx ends.
func (f *fakeServer) delay(ctx context.Context, method string) error {
	d := f.st.cfg().Latency[method].sample()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// statusWithReasons is a status of code carrying one ErrorInfo per reason
// (as the libvirt provider builds them).
func statusWithReasons(code codes.Code, msg string, meta map[string]string, reasons ...string) error {
	st := status.New(code, msg)
	withInfo := st
	for _, r := range reasons {
		next, err := withInfo.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain, Metadata: meta})
		if err != nil {
			return st.Err()
		}
		withInfo = next
	}
	return withInfo.Err()
}

// hostUnavailable is the clustered HOST_UNAVAILABLE answer for host.
func hostUnavailable(host string) error {
	return statusWithReasons(codes.Unavailable, fmt.Sprintf("host %s is unavailable", host),
		map[string]string{contracts.HostUnavailableHostKey: host}, contracts.HostUnavailableReason)
}

// routedHost resolves the host a per-VM call names: an empty id is
// InvalidArgument (never defaulted), an unknown or unreachable host is
// HOST_UNAVAILABLE.
func (f *fakeServer) routedHost(id string) (*hostAgent, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "target_host_id is required by a clustered provider")
	}
	h := f.st.host(id)
	if h == nil || f.st.unreachable(id) {
		return nil, hostUnavailable(id)
	}
	return h, nil
}

// execute holds one of h's execution slots while fn runs, after the
// method's latency; the wait for a slot is recorded.
func (f *fakeServer) execute(ctx context.Context, h *hostAgent, method string, fn func() error) error {
	waitStart := time.Now()
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
	f.m.slotWait.WithLabelValues(method).Observe(time.Since(waitStart).Seconds())
	defer func() { <-h.slots }()
	if err := f.delay(ctx, method); err != nil {
		return err
	}
	return fn()
}

// Validate reports the provider healthy.
func (f *fakeServer) Validate(ctx context.Context, _ *providerv1.ValidateRequest) (resp *providerv1.ValidateResponse, err error) {
	defer f.observe(methodValidate, time.Now(), &err)
	if err = f.delay(ctx, methodValidate); err != nil {
		return nil, err
	}
	return &providerv1.ValidateResponse{Ok: true, Message: fmt.Sprintf("fake clustered provider, %d hosts", len(f.st.hostIDs()))}, nil
}

// GetCapabilities reports only what the fake implements.
func (f *fakeServer) GetCapabilities(ctx context.Context, _ *providerv1.GetCapabilitiesRequest) (resp *providerv1.GetCapabilitiesResponse, err error) {
	defer f.observe(methodCaps, time.Now(), &err)
	if err = f.delay(ctx, methodCaps); err != nil {
		return nil, err
	}
	return &providerv1.GetCapabilitiesResponse{
		SupportsClustering:      true,
		SupportsListOwnerFilter: true,
		SupportsOwnerRebind:     true,
		SupportedDiskTypes:      []string{"raw"},
		SupportedNetworkTypes:   []string{"virtio"},
	}, nil
}

// hostInfo is the inventory answer for host id.
func (f *fakeServer) hostInfo(id string) *providerv1.HostInfo {
	c := f.st.cfg()
	health := providerv1.HostHealth_HOST_HEALTH_READY
	if f.st.unreachable(id) {
		health = providerv1.HostHealth_HOST_HEALTH_NOT_READY
	}
	storage := int64(4) << 40
	return &providerv1.HostInfo{
		Id: id, Address: "agent://" + id, AllocatableCpu: c.HostCPU, AllocatableMemMib: c.HostMemMiB,
		AllocatableStorage: storage, Health: health, CpuModel: "fake", MachineTypes: []string{"fake-q35"},
		EmulatorVersion: "fake-agent/1.0",
	}
}

// GetHostInfo answers one host's inventory.
func (f *fakeServer) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (resp *providerv1.HostInfo, err error) {
	defer f.observe(methodHostInfo, time.Now(), &err)
	if f.st.host(req.GetHostId()) == nil {
		return nil, status.Errorf(codes.NotFound, "host %s is not in this provider's inventory", req.GetHostId())
	}
	if f.st.unreachable(req.GetHostId()) {
		return nil, hostUnavailable(req.GetHostId())
	}
	if err = f.delay(ctx, methodHostInfo); err != nil {
		return nil, err
	}
	return f.hostInfo(req.GetHostId()), nil
}

// ListHosts answers every served host's inventory.
func (f *fakeServer) ListHosts(ctx context.Context, _ *providerv1.ListHostsRequest) (resp *providerv1.ListHostsResponse, err error) {
	defer f.observe(methodListHosts, time.Now(), &err)
	if err = f.delay(ctx, methodListHosts); err != nil {
		return nil, err
	}
	out := &providerv1.ListHostsResponse{}
	for _, id := range f.st.hostIDs() {
		out.Hosts = append(out.Hosts, f.hostInfo(id))
	}
	return out, nil
}

// classSize reads the VM size from the Create's class_json.
func classSize(classJSON string) (int32, int64) {
	var c contracts.VMClass
	if err := json.Unmarshal([]byte(classJSON), &c); err != nil || c.CPU == 0 {
		return workload.Classes[0].CPU, workload.Classes[0].MemoryMiB
	}
	return c.CPU, int64(c.MemoryMiB)
}

// Create creates the VM on target_host_id, owned by req.owner.
func (f *fakeServer) Create(ctx context.Context, req *providerv1.CreateRequest) (resp *providerv1.CreateResponse, err error) {
	defer f.observe(methodCreate, time.Now(), &err)
	owner := req.GetOwner()
	if owner.GetUid() == "" || owner.GetNamespace() == "" || owner.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "a clustered Create needs the owner identity")
	}
	h, err := f.routedHost(req.GetTargetHostId())
	if err != nil {
		return nil, err
	}
	c := f.st.cfg()
	if c.CreateFailBefore > 0 && rand.Float64() < c.CreateFailBefore { // #nosec G404 -- fault injection
		f.m.faults.WithLabelValues("create_fail_before").Inc()
		return nil, hostUnavailable(req.GetTargetHostId())
	}
	id := workload.DomainID(owner.GetNamespace(), owner.GetName())
	cpu, mem := classSize(req.GetClassJson())
	err = f.execute(ctx, h, methodCreate, func() error {
		h.mu.Lock()
		if existing, ok := h.vms[id]; ok {
			h.mu.Unlock()
			if existing.OwnerUID == "" && existing.Seeded {
				f.st.adopt(existing, owner.GetUid())
				f.m.createRepeat.Inc()
				return nil
			}
			if existing.OwnerUID == owner.GetUid() {
				f.m.createRepeat.Inc()
				return nil
			}
			return status.Errorf(codes.AlreadyExists, "VM %s on host %s belongs to another owner", id, req.GetTargetHostId())
		}
		rec := &vmRecord{
			ID: id, Host: req.GetTargetHostId(), OwnerUID: owner.GetUid(), Namespace: owner.GetNamespace(), Name: owner.GetName(),
			CPU: cpu, MemoryMiB: mem, Power: powerOn, IP: workload.IPFor(owner.GetNamespace(), owner.GetName()), Created: time.Now(),
		}
		h.vms[id] = rec
		h.mu.Unlock()
		f.st.addRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if c.CreateLostReply > 0 && rand.Float64() < c.CreateLostReply { // #nosec G404 -- fault injection
		f.m.faults.WithLabelValues("create_lost_reply").Inc()
		return nil, statusWithReasons(codes.Unavailable, "create reply lost (injected)", nil,
			contracts.OutcomeUnknownRetryReason, contracts.VMOperationFailedReason)
	}
	return &providerv1.CreateResponse{Id: id}, nil
}

// ownedRecord returns the record id on h if owner may see it. A seeded
// record without an owner adopts the first owner whose namespace/name match.
func (f *fakeServer) ownedRecord(h *hostAgent, id string, owner *providerv1.ObjectIdentity) *vmRecord {
	// A VM whose Create is still pending is addressed by its bare name plus
	// its owner (the manager does not know the host-side name yet;
	// pendingCreateRef). Resolve it through the owner, as the libvirt
	// provider does.
	if owner.GetNamespace() != "" && id == owner.GetName() {
		id = workload.DomainID(owner.GetNamespace(), owner.GetName())
	}
	h.mu.Lock()
	rec, ok := h.vms[id]
	h.mu.Unlock()
	if !ok {
		return nil
	}
	if owner.GetUid() == "" {
		return rec
	}
	if rec.OwnerUID == "" && rec.Namespace == owner.GetNamespace() && rec.Name == owner.GetName() {
		f.st.adopt(rec, owner.GetUid())
	}
	if rec.OwnerUID != owner.GetUid() {
		return nil
	}
	return rec
}

// Describe reports the VM on target_host_id if req.owner owns it.
func (f *fakeServer) Describe(ctx context.Context, req *providerv1.DescribeRequest) (resp *providerv1.DescribeResponse, err error) {
	defer f.observe(methodDescribe, time.Now(), &err)
	h, err := f.routedHost(req.GetTargetHostId())
	if err != nil {
		return nil, err
	}
	if err = f.delay(ctx, methodDescribe); err != nil {
		return nil, err
	}
	rec := f.ownedRecord(h, req.GetId(), req.GetOwner())
	if rec == nil {
		return &providerv1.DescribeResponse{Exists: false}, nil
	}
	h.mu.Lock()
	out := &providerv1.DescribeResponse{Exists: true, PowerState: rec.Power, MaxMemoryMib: rec.MemoryMiB, Vcpus: rec.CPU}
	if f.st.cfg().ReportIPs && rec.Power == powerOn {
		out.Ips = []string{rec.IP}
	}
	h.mu.Unlock()
	return out, nil
}

// Delete removes the VM on target_host_id if req.owner owns it; anything
// else is NotFound and untouched.
func (f *fakeServer) Delete(ctx context.Context, req *providerv1.DeleteRequest) (resp *providerv1.TaskResponse, err error) {
	defer f.observe(methodDelete, time.Now(), &err)
	h, err := f.routedHost(req.GetTargetHostId())
	if err != nil {
		return nil, err
	}
	err = f.execute(ctx, h, methodDelete, func() error {
		rec := f.ownedRecord(h, req.GetId(), req.GetOwner())
		if rec == nil {
			return status.Errorf(codes.NotFound, "VM %s not found on host %s", req.GetId(), req.GetTargetHostId())
		}
		h.mu.Lock()
		delete(h.vms, rec.ID)
		h.mu.Unlock()
		f.st.removeRecord(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &providerv1.TaskResponse{}, nil
}

// Power changes the VM's power state.
func (f *fakeServer) Power(ctx context.Context, req *providerv1.PowerRequest) (resp *providerv1.TaskResponse, err error) {
	defer f.observe(methodPower, time.Now(), &err)
	h, err := f.routedHost(req.GetTargetHostId())
	if err != nil {
		return nil, err
	}
	err = f.execute(ctx, h, methodPower, func() error {
		rec := f.ownedRecord(h, req.GetId(), req.GetOwner())
		if rec == nil {
			return status.Errorf(codes.NotFound, "VM %s not found on host %s", req.GetId(), req.GetTargetHostId())
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		switch req.GetOp() {
		case providerv1.PowerOp_POWER_OP_ON, providerv1.PowerOp_POWER_OP_REBOOT:
			rec.Power = powerOn
		case providerv1.PowerOp_POWER_OP_OFF, providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
			rec.Power = powerOff
		default:
			return status.Errorf(codes.InvalidArgument, "power op %s", req.GetOp())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &providerv1.TaskResponse{}, nil
}

// ListVMs lists the VMs of one owner (namespace/name filter) or of every host.
func (f *fakeServer) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (resp *providerv1.ListVMsResponse, err error) {
	defer f.observe(methodListVMs, time.Now(), &err)
	if err = f.delay(ctx, methodListVMs); err != nil {
		return nil, err
	}
	var recs []vmRecord
	filtered := req.GetOwnerNamespace() != "" && req.GetOwnerName() != ""
	if filtered {
		recs = f.st.owned(req.GetOwnerNamespace(), req.GetOwnerName())
	} else {
		recs = f.st.snapshot()
	}
	out := &providerv1.ListVMsResponse{OwnerFilterApplied: filtered}
	for _, r := range recs {
		out.Vms = append(out.Vms, &providerv1.VMInfo{
			Id: r.ID, Name: r.Name, PowerState: r.Power, Cpu: r.CPU, MemoryMib: r.MemoryMiB,
			HostId: r.Host, OwnerNamespace: r.Namespace, OwnerName: r.Name,
		})
	}
	out.UnreachableHostIds = append(out.UnreachableHostIds, f.st.cfg().UnreachableHosts...)
	return out, nil
}

// TaskStatus: every fake operation is synchronous, so a task is complete.
func (f *fakeServer) TaskStatus(ctx context.Context, _ *providerv1.TaskStatusRequest) (resp *providerv1.TaskStatusResponse, err error) {
	defer f.observe(methodTaskStatus, time.Now(), &err)
	if err = f.delay(ctx, methodTaskStatus); err != nil {
		return nil, err
	}
	return &providerv1.TaskStatusResponse{Done: true}, nil
}
