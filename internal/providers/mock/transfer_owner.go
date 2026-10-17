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

package mock

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TransferOwner stamps the mock VM req.id with req.owner, compare-and-swap
// under the provider's lock: the VM's recorded owner must be absent, the new
// owner's (an idempotent retry) or listed replaceable
// (contracts.OwnerTransferDecision), or the call is refused (AlreadyExists)
// and nothing changes. A VM that does not exist is NotFound. The mock stamps
// only here (its Create records no owner); ListVMs reports the stamp as
// provider_raw "owner_uid", as the real providers do, so the manager's
// single-host adoption can be exercised end to end against it.
func (p *Provider) TransferOwner(_ context.Context, req *providerv1.TransferOwnerRequest) (*providerv1.TransferOwnerResponse, error) {
	owner := contracts.ObjectIdentity{UID: req.GetOwner().GetUid(), Namespace: req.GetOwner().GetNamespace(), Name: req.GetOwner().GetName()}
	switch {
	case req.GetId() == "":
		return nil, status.Error(codes.InvalidArgument, "TransferOwner requires the VM's id")
	case strings.TrimSpace(req.GetTargetHostId()) != "":
		return nil, status.Error(codes.InvalidArgument, "TransferOwner: the mock provider is not clustered and takes no target host")
	case owner.UID == "" || owner.Namespace == "" || owner.Name == "":
		return nil, status.Error(codes.InvalidArgument, "TransferOwner requires the new owner's uid, namespace and name")
	}
	if p.shouldFail("transferowner") {
		return nil, status.Error(codes.Unavailable, "mock provider configured to fail owner transfers")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	vm, ok := p.vms[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "mock VM %q not found; its owner was not changed", req.GetId())
	}
	var recorded []contracts.ObjectIdentity
	if !vm.Owner.IsZero() {
		recorded = append(recorded, vm.Owner)
	}
	if _, err := contracts.OwnerTransferDecision(owner, recorded, req.GetReplaceableOwnerUids()); err != nil {
		p.logger.Warn("Refusing to transfer the owner of a mock VM", "vm_id", vm.ID, "reason", err.Error())
		return nil, status.Errorf(codes.AlreadyExists,
			"mock VM %q is owned by another VirtualMachine; its owner was not changed", req.GetId())
	}
	vm.Owner = owner
	return &providerv1.TransferOwnerResponse{}, nil
}
