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

package fake

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
)

const taskPrefix = "host-task/v1/"

var hostIDRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

type vm struct {
	name    string
	owner   clustered.Identity
	running bool
}

type hostState struct {
	reachable bool
	stuck     bool // StickyUnreachable
	vms       map[string]*vm
	tasks     map[string]bool // host-local task -> done
}

// world is the state that lives on the hosts: it survives a restart of the
// provider.
type world struct {
	mu      sync.Mutex
	order   []string
	hosts   map[string]*hostState
	dialed  []string
	taskSeq int
	ready   bool
	deleted []deletedVM // for RestoresDeletedOnRestart
}

type deletedVM struct {
	host string
	v    vm
}

// provider is the fake gRPC service. Its own fields are the in-memory state
// of the provider process.
type provider struct {
	providerv1.UnimplementedProviderServer
	w          *world
	violations map[Violation]bool
	async      bool
	notOwned   bool // declares and attaches VM_NOT_OWNED
	capsCalls  int
	memTasks   map[string]bool // TasksInMemory
	served     int
}

func (p *provider) v(x Violation) bool { return p.violations[x] }

func (p *provider) dial(host string) {
	p.w.dialed = append(p.w.dialed, host)
}

func hostUnavailable(p *provider, host, msg string) error {
	if p.v(HostDownNoReason) {
		return status.Error(codes.Unavailable, msg)
	}
	domain := contracts.HostUnavailableErrorDomain
	if p.v(HostUnavailableWrongDomain) {
		domain = "example.com"
	}
	md := map[string]string{contracts.HostUnavailableHostKey: host}
	if p.v(HostUnavailableNoHost) {
		md = nil
	}
	return withInfo(codes.Unavailable, msg, contracts.HostUnavailableReason, domain, md)
}

func withInfo(c codes.Code, msg, reason, domain string, md map[string]string) error {
	st, err := status.New(c, msg).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain, Metadata: md})
	if err != nil {
		return status.Error(c, msg)
	}
	return st.Err()
}

// gate refuses every call while the provider is not ready.
func (p *provider) gate() error {
	if p.w.ready {
		return nil
	}
	if p.v(NotReadyLooksHostScoped) {
		return withInfo(codes.Unavailable, "provider not ready", contracts.HostUnavailableReason, contracts.HostUnavailableErrorDomain, nil)
	}
	return status.Error(codes.Unavailable, "provider not ready")
}

// normalize applies NormalizeHostIDs: a sloppy parser that accepts what it
// should refuse.
func normalizeHost(s string) string {
	s = strings.ToLower(path.Base(strings.TrimSpace(s)))
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

// route resolves a request's target_host_id to a host the call may use. The
// caller holds w.mu.
func (p *provider) route(raw string, ignoreReach bool) (string, *hostState, error) {
	if p.v(DialRequestedHostFirst) && strings.TrimSpace(raw) != "" {
		p.dial(raw)
	}
	host := strings.TrimSpace(raw)
	if host == "" {
		if !p.v(AllowEmptyHost) {
			return "", nil, status.Error(codes.InvalidArgument, "target_host_id is required by a clustered provider")
		}
		host = p.w.order[0]
	}
	if p.v(NormalizeHostIDs) {
		host = normalizeHost(host)
	}
	if len(host) > 253 || !hostIDRE.MatchString(host) {
		return "", nil, status.Error(codes.InvalidArgument, "target_host_id is not a valid host id")
	}
	hs := p.w.hosts[host]
	if hs == nil {
		if p.v(UnknownHostNotFound) {
			return "", nil, status.Error(codes.NotFound, "host not found")
		}
		if p.v(UnknownHostNoReason) {
			return "", nil, status.Error(codes.Unavailable, "unknown host")
		}
		return "", nil, hostUnavailable(p, host, "host is not in this provider's registry")
	}
	if !p.v(DialRequestedHostFirst) {
		p.dial(host)
	}
	if p.v(HostDownBreaksPeers) {
		for _, h := range p.w.hosts {
			if !h.reachable && !ignoreReach {
				return "", nil, hostUnavailable(p, host, "a host of the cluster is unavailable")
			}
		}
	}
	if !hs.reachable {
		hs.stuck = true
	}
	if (!hs.reachable || (hs.stuck && p.v(StickyUnreachable))) && !ignoreReach {
		if p.v(HostDownAsNotFound) {
			return "", nil, status.Error(codes.NotFound, "host not found")
		}
		return "", nil, hostUnavailable(p, host, "host is unreachable")
	}
	return host, hs, nil
}

// authorize reports whether owner may act on v.
func (p *provider) authorize(v *vm, owner *providerv1.ObjectIdentity, mutate bool) bool {
	if mutate && p.v(IgnoreOwnerMutate) {
		return true
	}
	if !mutate && p.v(IgnoreOwnerDescribe) {
		return true
	}
	if owner.GetUid() == "" {
		return p.v(MissingOwnerAuthorizes)
	}
	if v.owner.UID == "" {
		return p.v(UnstampedAuthorizes)
	}
	return v.owner.UID == owner.GetUid()
}

// refuse is the answer for a mutation on a VM that is absent (v == nil) or not
// the requester's.
func (p *provider) refuse(v *vm) error {
	if v != nil && p.v(ForeignWrongCode) {
		return status.Error(codes.PermissionDenied, "not permitted")
	}
	msg := "VM not found on host"
	if v != nil && p.v(OwnerLeak) {
		msg = fmt.Sprintf("VM is owned by %s/%s", v.owner.Namespace, v.owner.Name)
	}
	attach := p.notOwned && v != nil && !p.v(NotOwnedReasonMissing)
	if p.notOwned && v == nil && p.v(NotOwnedReasonOnAbsent) {
		attach = true
	}
	if attach {
		return withInfo(codes.NotFound, msg, contracts.VMNotOwnedReason, contracts.ErrorInfoDomain, nil)
	}
	return status.Error(codes.NotFound, msg)
}

func (p *provider) findVM(host string, hs *hostState, id string) *vm {
	if v := hs.vms[id]; v != nil {
		return v
	}
	if p.v(FallbackToAnyHost) {
		for _, h := range p.w.order {
			if h != host && p.w.hosts[h].reachable {
				if v := p.w.hosts[h].vms[id]; v != nil {
					return v
				}
			}
		}
	}
	return nil
}

func powerName(running bool) string {
	if running {
		return "On"
	}
	return "Off"
}

// Describe implements the owner-checked, routed Describe.
func (p *provider) Describe(_ context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	v := p.findVM(host, hs, req.Id)
	if v == nil || !p.authorize(v, req.Owner, false) {
		return &providerv1.DescribeResponse{Exists: false}, nil
	}
	resp := &providerv1.DescribeResponse{Exists: true, PowerState: powerName(v.running), Vcpus: 2}
	if p.v(DescribeFlaps) {
		v.running = !v.running
	}
	return resp, nil
}

// newTask records a host-local task and returns its wire reference.
func (p *provider) newTask(host string, hs *hostState) *providerv1.TaskRef {
	p.w.taskSeq++
	inner := "job-" + strconv.Itoa(p.w.taskSeq)
	if p.v(TasksInMemory) {
		p.memTasks[host+"/"+inner] = false
	} else {
		hs.tasks[inner] = false
	}
	if p.v(TaskNotHostEncoded) {
		return &providerv1.TaskRef{Id: inner}
	}
	return &providerv1.TaskRef{Id: taskPrefix + host + "/" + inner}
}

// Power implements the owner-checked, routed Power.
func (p *provider) Power(_ context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	host, hs, err := p.route(req.TargetHostId, p.v(HostDownMutatesAnyway))
	if err != nil {
		return nil, err
	}
	v := p.findVM(host, hs, req.Id)
	if v == nil || !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	want := req.Op == providerv1.PowerOp_POWER_OP_ON || req.Op == providerv1.PowerOp_POWER_OP_REBOOT
	if p.v(PowerRepeatFails) && want == v.running && req.Op != providerv1.PowerOp_POWER_OP_REBOOT {
		return nil, status.Error(codes.FailedPrecondition, "VM is already in that state")
	}
	if !p.v(PowerNoEffect) {
		v.running = want
	}
	if p.v(HostDownMutatesAnyway) && !hs.reachable {
		return nil, hostUnavailable(p, host, "host is unreachable")
	}
	resp := &providerv1.TaskResponse{}
	if p.async {
		resp.Task = p.newTask(host, hs)
	}
	return resp, nil
}

// Delete implements the owner-checked, routed Delete.
func (p *provider) Delete(_ context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	v := hs.vms[req.Id]
	if v == nil {
		if p.v(DeleteTwiceFails) {
			return nil, status.Error(codes.Internal, "delete failed")
		}
		return nil, p.refuse(nil)
	}
	if !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	p.w.deleted = append(p.w.deleted, deletedVM{host: host, v: *v})
	delete(hs.vms, req.Id)
	resp := &providerv1.TaskResponse{}
	if p.async {
		resp.Task = p.newTask(host, hs)
	}
	return resp, nil
}

// Reconfigure implements the owner-checked, routed Reconfigure.
func (p *provider) Reconfigure(_ context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	v := p.findVM(host, hs, req.Id)
	if v == nil || !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	return &providerv1.TaskResponse{HonestResult: true}, nil
}

// TaskStatus implements the routed TaskStatus.
func (p *provider) TaskStatus(_ context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	ref := req.GetTask().GetId()
	rest, ok := strings.CutPrefix(ref, taskPrefix)
	host, inner, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || inner == "" || len(ref) > 1024 || !hostIDRE.MatchString(host) {
		if p.v(TaskAcceptsMalformed) {
			return &providerv1.TaskStatusResponse{Done: true}, nil
		}
		return nil, status.Error(codes.InvalidArgument, "task reference is not one this provider issued")
	}
	if p.v(TaskDialsUnknownHost) {
		p.dial(host)
	}
	hs := p.w.hosts[host]
	if hs == nil {
		if p.v(TaskUnknownHostDone) {
			return &providerv1.TaskStatusResponse{Done: true}, nil
		}
		return nil, status.Error(codes.NotFound, "task host is not in this provider's registry")
	}
	if p.v(TaskWrongHost) {
		for _, h := range p.w.order {
			if p.w.hosts[h].reachable {
				host, hs = h, p.w.hosts[h]
				break
			}
		}
	}
	if !hs.reachable {
		return nil, hostUnavailable(p, host, "host is unreachable")
	}
	p.dial(host)
	var done, known bool
	if p.v(TasksInMemory) {
		done, known = p.memTasks[host+"/"+inner]
	} else {
		done, known = hs.tasks[inner]
	}
	if !known {
		if p.v(TaskForgedInnerDone) {
			return &providerv1.TaskStatusResponse{Done: true}, nil
		}
		return nil, status.Error(codes.NotFound, "no such task on the host")
	}
	return &providerv1.TaskStatusResponse{Done: done}, nil
}

// ListVMs lists the VMs of every host.
func (p *provider) ListVMs(_ context.Context, _ *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	resp := &providerv1.ListVMsResponse{}
	seenName := map[string]bool{}
	for _, host := range p.w.order {
		hs := p.w.hosts[host]
		if !hs.reachable {
			hs.stuck = true
		}
		if !hs.reachable || (hs.stuck && p.v(StickyUnreachable)) {
			if p.v(ListFailsOnDownHost) {
				return nil, hostUnavailable(p, host, "host is unreachable")
			}
			if !p.v(ListDropsUnreachable) {
				resp.UnreachableHostIds = append(resp.UnreachableHostIds, host)
			}
			continue
		}
		ids := make([]string, 0, len(hs.vms))
		for id := range hs.vms {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if p.v(ListDedupesByName) && seenName[id] {
				continue
			}
			seenName[id] = true
			info := &providerv1.VMInfo{Id: id, Name: hs.vms[id].name, HostId: host,
				OwnerNamespace: hs.vms[id].owner.Namespace, OwnerName: hs.vms[id].owner.Name,
				PowerState: powerName(hs.vms[id].running)}
			if p.v(ListNoHostID) {
				info.HostId = ""
			}
			resp.Vms = append(resp.Vms, info)
		}
	}
	return resp, nil
}

// GetCapabilities reports the clustered capability set.
func (p *provider) GetCapabilities(_ context.Context, _ *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.capsCalls++
	caps := &providerv1.GetCapabilitiesResponse{
		SupportsClustering:     !p.v(CapsNoClustering),
		SupportsSnapshots:      true,
		SupportsRoutedAdoption: true,
	}
	if p.v(CapsUnstable) {
		caps.SupportsSnapshots = p.capsCalls%2 == 0
	}
	if p.v(CapsAdvertiseUnrouted) {
		caps.SupportsDiskExport = true
		caps.SupportsRoutedClone = true
	}
	return caps, nil
}

// SnapshotCreate is routed and owner-checked.
func (p *provider) SnapshotCreate(_ context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	if v := p.findVM(host, hs, req.VmId); v == nil || !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	return &providerv1.SnapshotCreateResponse{SnapshotId: "snap-1"}, nil
}

// SnapshotDelete is routed and owner-checked.
func (p *provider) SnapshotDelete(_ context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	if v := p.findVM(host, hs, req.VmId); v == nil || !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	return &providerv1.TaskResponse{}, nil
}

// SnapshotRevert is routed and owner-checked.
func (p *provider) SnapshotRevert(_ context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	host, hs, err := p.route(req.TargetHostId, false)
	if err != nil {
		return nil, err
	}
	if v := p.findVM(host, hs, req.VmId); v == nil || !p.authorize(v, req.Owner, true) {
		return nil, p.refuse(v)
	}
	return &providerv1.TaskResponse{}, nil
}

// Create makes a VM on the target host and stamps its owner.
func (p *provider) Create(_ context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	target := req.TargetHostId
	if p.v(CreateIgnoresHost) {
		target = p.w.order[0]
	}
	host, hs, err := p.route(target, p.v(CreateIgnoresReachability))
	if err != nil {
		return nil, err
	}
	id := req.Name
	if old := hs.vms[id]; old != nil {
		switch {
		case old.owner.UID != "" && old.owner.UID == req.GetOwner().GetUid():
			if p.v(CreateDuplicates) {
				id = req.Name + "-2"
			} else {
				return &providerv1.CreateResponse{Id: id}, nil
			}
		case p.v(CreateRebindsForeign):
			return &providerv1.CreateResponse{Id: id}, nil
		default:
			return nil, status.Error(codes.AlreadyExists, "a VM of that name exists and is not this owner's")
		}
	}
	owner := clustered.Identity{UID: req.GetOwner().GetUid(), Namespace: req.GetOwner().GetNamespace(), Name: req.GetOwner().GetName()}
	if p.v(CreateNoOwnerStamp) {
		owner = clustered.Identity{}
	}
	hs.vms[id] = &vm{name: req.Name, owner: owner}
	_ = host
	return &providerv1.CreateResponse{Id: id}, nil
}

// ListHosts reports the host inventory.
func (p *provider) ListHosts(_ context.Context, _ *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err := p.gate(); err != nil {
		return nil, err
	}
	resp := &providerv1.ListHostsResponse{}
	for _, host := range p.w.order {
		hs := p.w.hosts[host]
		if !hs.reachable && p.v(InventoryDropsDownHost) {
			continue
		}
		health := providerv1.HostHealth_HOST_HEALTH_READY
		if !hs.reachable && !p.v(InventoryDownIsReady) {
			health = providerv1.HostHealth_HOST_HEALTH_NOT_READY
		}
		resp.Hosts = append(resp.Hosts, &providerv1.HostInfo{Id: host, Health: health})
	}
	return resp, nil
}

// GetHostInfo reports one host.
func (p *provider) GetHostInfo(_ context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	hs := p.w.hosts[req.HostId]
	if hs == nil {
		if p.v(GetHostInfoUnknownOK) {
			return &providerv1.HostInfo{Id: req.HostId}, nil
		}
		return nil, status.Error(codes.NotFound, "no such host")
	}
	health := providerv1.HostHealth_HOST_HEALTH_READY
	if !hs.reachable {
		health = providerv1.HostHealth_HOST_HEALTH_NOT_READY
	}
	return &providerv1.HostInfo{Id: req.HostId, Health: health}, nil
}
