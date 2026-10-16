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
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// singleHostTransferLabel names the host of a single-host provider in
// TransferOwner's messages and logs. The provider's endpoint is never put on
// the wire; the provider log records which provider answered.
const singleHostTransferLabel = hostconn.HostID("single-host")

// transferOwnerSingleHost is TransferOwner on a SINGLE-HOST provider: the
// clustered core (transferOwnerOn) on the provider's one connection, so the
// same compare-and-swap applies — the domain must still be named req.VM.ID and
// carry req.ExpectedUUID (the UUID the single-host ListVMs reports), every
// stamp on it (both definitions of an active domain) must be the new owner's
// or listed replaceable, and the stamp is written with `virsh metadata` and
// read back. Transfers are serialized in the provider process; two provider
// processes fronting the same host are not (see the file comment of
// routed_transfer_owner.go).
//
// The request must not name a host: a single-host provider has none to route
// to, and silently ignoring one would hide a manager that believes the
// Provider is clustered.
func (p *Provider) transferOwnerSingleHost(ctx context.Context, req contracts.TransferOwnerRequest) error {
	if strings.TrimSpace(req.VM.HostID) != "" {
		return contracts.NewInvalidSpecError("TransferOwner: a single-host libvirt provider takes no target host", nil)
	}
	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}
	unlock, err := p.transferLocks.lock(ctx, string(singleHostTransferLabel))
	if err != nil {
		return contracts.NewRetryableError("wait for another owner transfer on the host", err)
	}
	defer unlock()
	return transferOwnerOn(ctx, p.virshProvider, singleHostTransferLabel, req)
}
