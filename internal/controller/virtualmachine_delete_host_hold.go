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
	"fmt"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clustered delete held because its host does not answer (V3a).
//
// The provider tells the manager which host it could not reach only when that
// host is the one the call was routed to: the HOST_UNAVAILABLE ErrorInfo then
// names it (contracts.UnreachableHost), and an answer that the host stopped
// answering during the call (contracts.IsHostConnectionLost, R25) is always
// about that host. In both cases the DeleteBlocked=True/HostUnreachable
// message names the VM's own host — its Host CR name, status.placement.host
// (or the pending host of a create in flight), which the VM's readers already
// see — never an endpoint, a URI or an address. A HOST_UNAVAILABLE that does
// not name the VM's host (the cluster-wide disk guard could not reach a host,
// possibly another one; an older provider) keeps the constant message that
// names no host. The hold is lifted as before: the finalizer is removed when
// the delete completes, and a later attempt that fails for another reason
// removes DeleteBlocked (clearStaleDeleteHold).

// hostUnreachableDeleteFmt is the DeleteBlocked/HostUnreachable message of a
// clustered delete whose own host could not be reached before anything was
// deleted: the host, the backoff cap and deleteBlockedEscape.
const hostUnreachableDeleteFmt = "Delete held: this VirtualMachine's host %s could not be reached, so nothing was " +
	"deleted. The finalizer is kept; the delete is retried with a backoff of up to %s and completes once the host " +
	"answers. %s"

// hostLostDeleteFmt is the DeleteBlocked/HostUnreachable message of a
// clustered delete whose host stopped answering after the delete may have
// changed something (R25: an unknown outcome).
const hostLostDeleteFmt = "Delete held: this VirtualMachine's host %s stopped answering during the delete, so it is " +
	"not known how far the delete got. The finalizer is kept; the delete is retried with a backoff of up to %s and " +
	"completes once the host answers. %s"

// deleteHostHold returns the DeleteBlocked reason and message of a clustered
// delete the provider did not complete because a host could not be reached
// (HOST_UNAVAILABLE), could not be checked (VM_DISK_CHECK_FAILED), or stopped
// answering during the call (HOST_CONNECTION_LOST). See the comment above for
// when the message names the VM's host.
func deleteHostHold(vm *infravirtrigaudiov1beta1.VirtualMachine, err error) (reason, msg string) {
	host := placementHostForLog(vm)
	switch {
	case contracts.IsHostUnavailable(err):
		if host != "" && contracts.UnreachableHost(err) == host {
			return k8s.ReasonHostUnreachable, fmt.Sprintf(hostUnreachableDeleteFmt, host, blockedRetryMax, deleteBlockedEscape)
		}
		return k8s.ReasonHostUnreachable, deleteBlockedMessages[k8s.ReasonHostUnreachable]
	case contracts.IsHostConnectionLost(err):
		if host == "" {
			return k8s.ReasonHostUnreachable, deleteBlockedMessages[k8s.ReasonHostUnreachable]
		}
		if contracts.IsNothingDone(err) {
			return k8s.ReasonHostUnreachable, fmt.Sprintf(hostUnreachableDeleteFmt, host, blockedRetryMax, deleteBlockedEscape)
		}
		return k8s.ReasonHostUnreachable, fmt.Sprintf(hostLostDeleteFmt, host, blockedRetryMax, deleteBlockedEscape)
	}
	return k8s.ReasonDiskCheckFailed, deleteBlockedMessages[k8s.ReasonDiskCheckFailed]
}

// heldForUnreachableHost reports whether a failed clustered Delete is held by
// retainForUncheckedDelete: the provider did not perform it because a host
// could not be reached or checked, or the VM's host stopped answering during
// it.
func heldForUnreachableHost(err error) bool {
	return contracts.IsHostUnavailable(err) || contracts.IsVMDiskCheckFailed(err) || contracts.IsHostConnectionLost(err)
}
