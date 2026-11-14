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

// Package cloudhypervisor is the clustered Provider that fronts per-host
// agents (cmd/vrtg-agent): it routes each per-VM call to the agent of the
// host the call names and forwards the call's request message unchanged, so
// that the agent, which holds the pinned grant anchors, is the only party
// that judges an operation grant. It is for development builds only: every
// file of the package except this one carries the virtrigaud_dev build tag,
// so a release build links none of it.
//
// # Routing
//
// A mutating call (Create, Delete, Power, Reconfigure) must carry an
// operation envelope and nothing else: a call without one is refused with
// IDENTITY_REQUIRED, and one with an outer field beside it with
// INVALID_ARGUMENT, before any host is dialed. Such a call is routed by the
// target_host_id of a decoded copy of the request inside the envelope, never
// by an outer field. The outer request is forwarded byte for byte as it
// arrived (the Provider's gRPC service reads it as raw bytes), on the same
// method, to the host's agent. Describe, GetHostInfo and a TaskStatus that
// asks for a signed answer are routed by their host field and forwarded
// unchanged; their answers are relayed unchanged. The task reference a
// mutating call returns is host-encoded (internal/clustered), so that a
// TaskStatus that names only a task reaches the host that issued it.
//
// # Hosts
//
// The hosts come from the host inventory (internal/clustered/hostsecret),
// read from a file (internal/clustered/inventoryfile). Every entry must carry
// agent TLS material (hostsecret.ValidateAgentHost); any other entry is not
// routed. A host's agent is accepted only when its certificate chains to the
// entry's CA bundle for server authentication, has the host certificate
// profile, carries exactly the entry's host URI and one enrolment record,
// and certifies a key whose SubjectPublicKeyInfo SHA-256 is one of the
// entry's pins. A reload that removes a host, or changes its endpoint or
// TLS material, closes its connection at once.
//
// On every new connection to an agent the Provider calls agent.v1 Hello and
// verifies the host-signed answer against the connection's certificate; an
// agent whose API version is outside the Provider's version and the one
// before it is not routed (HOST_UNAVAILABLE, cause AgentIncompatible).
//
// Each host has its own circuit breaker. Only failures that say the host's
// agent cannot be reached count: a call the agent answered counts only when
// it answered Unavailable without a reason of the agent contract
// (contracts.PartAReasons). An open breaker refuses calls for its host only,
// with HOST_UNAVAILABLE, without dialing.
package cloudhypervisor
