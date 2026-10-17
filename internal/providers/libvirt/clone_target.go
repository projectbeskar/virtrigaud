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
	"fmt"
	"log"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// The single-host Clone's target name (rc2 R4).
//
// A Clone is re-sent when the manager never recorded its answer (a manager
// restart, a status write lost after the RPC, a call that ran past its
// deadline). A manager that knows the target VirtualMachine's UID sends it in
// CloneRequest.target_vm; the clone's domain is then stamped with that
// identity by the define that creates it, exactly as a Create stamps its
// domain. A re-sent Clone finds that domain under the target name and
// reports it as the clone (an idempotent success) instead of copying the
// source again. Any other domain under the target name — unstamped, stamped
// for another VirtualMachine, or with a stamp that cannot be read — is never
// bound or redefined: the Clone is refused with a Conflict (gRPC
// AlreadyExists), the class a Create uses for the same case. A request
// without a UID (a manager older than this provider) owns nothing, so a
// domain under its target name is refused the same way, and its clone is
// defined unstamped, as before.

// singleHostCloneHost labels the provider's one host in the owner check's log
// lines (checkDomainOwner logs the host it checked).
const singleHostCloneHost hostconn.HostID = "single-host"

// checkSingleHostCloneTarget decides a single-host Clone's target name,
// domainName, on vp's host before anything is read or copied for the clone.
// bound is true when a domain of that name is stamped with the target
// VirtualMachine's UID (req.TargetVM): an earlier attempt of this Clone
// defined it, and the caller reports it as the clone. A domain of that name
// that the target VirtualMachine does not own is a Conflict. A failure to
// list the host's domains or read the domain's stamp is retryable: nothing
// was done.
func checkSingleHostCloneTarget(ctx context.Context, vp *VirshProvider, domainName string, req contracts.CloneRequest) (bool, error) {
	own, err := checkDomainOwner(ctx, vp, singleHostCloneHost, domainName, req.TargetVM, "clone target")
	if err != nil {
		return false, err
	}
	switch {
	case !own.present:
		return false, nil
	case own.owned:
		log.Printf("INFO Clone target domain %s is already stamped with VirtualMachine %s/%s (uid %s); "+
			"an earlier attempt of this clone made it: treating the clone as done", domainName,
			req.TargetVM.Namespace, req.TargetVM.Name, req.TargetVM.UID)
		return true, nil
	}
	return false, contracts.NewConflictError(fmt.Sprintf(
		"libvirt domain %q already exists on the host and is not owned by the clone's target VirtualMachine; "+
			"nothing was cloned. Remove or rename that domain, or clone to another target name", domainName), nil)
}

// stampSingleHostClone stamps targetXML — the clone's definition, from which
// rewriteDomainXMLForClone removed every stamp the source carried — with the
// target VirtualMachine's identity when the request carries its UID. A
// request without a UID leaves the clone unstamped (a manager older than this
// provider). A definition that cannot be stamped is refused before anything is
// written.
func stampSingleHostClone(targetXML string, target contracts.ObjectIdentity) (string, error) {
	if target.IsZero() {
		return targetXML, nil
	}
	stamped, err := stampOwnerMetadata(targetXML, target)
	if err != nil {
		return "", contracts.NewInvalidSpecError("stamp the clone with its target VirtualMachine", err)
	}
	return stamped, nil
}
