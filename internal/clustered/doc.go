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

// Package clustered holds the hypervisor-neutral parts of a CLUSTERED
// provider (ADR-0007): one provider process that fronts N hosts, to which the
// operator names the host of every per-VM call (target_host_id, ADR-0007
// Addendum A).
//
// What belongs here is what every clustered provider must answer the same way,
// whatever its hypervisor and whatever it uses to reach a host (SSH and virsh
// for libvirt, a per-host agent for another provider):
//
//   - the host identity (HostID) and the routable host set (HostSet, Fronts);
//   - host-encoded task references (EncodeHostTaskRef, ParseHostTaskRef), the
//     NotFound answer for a task host the provider does not front, and the
//     UNKNOWN_TO_AGENT answer for a host-local task the host never issued
//     (ADR-0016 A7);
//   - the wire answers of a routed call: HOST_UNAVAILABLE naming the routed
//     host, VM_OPERATION_FAILED, OUTCOME_UNKNOWN_RETRY, HOST_CONNECTION_LOST,
//     and the choice between them by the kind of RPC and how far the call got
//     on its host before the host stopped answering (R25);
//   - the categorized routed errors (HostUnavailableError, OpError,
//     UnknownHostTaskError) and the wire form of the neutral ones (TypedStatus,
//     ProviderFailureStatus);
//   - the mapping of the transport-neutral host inventory onto the wire
//     (HostInfoToProto).
//
// What does NOT belong here is anything that knows how a host is reached or
// what runs on it: dialers, connection registries and leases, SSH host keys,
// virsh and qemu-img commands and their error texts, disk and staging logic,
// and the classification of a hypervisor's errors as "the host stopped
// answering". Those stay in the provider (for libvirt,
// internal/providers/libvirt and its hostconn package), which classifies its
// own failures and hands the neutral facts to the helpers here.
//
// The host-inventory Secret schema the Provider controller renders for a
// clustered provider lives in the sub-package hostsecret.
//
// This package must not import any provider package.
package clustered
