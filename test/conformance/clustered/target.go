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

// Package clustered is a reusable conformance suite for a clustered remote
// Provider: a Provider that fronts several hypervisor hosts and is told, on
// every per-VM call, which host to act on (ADR-0007 and its Addenda A and B).
//
// The suite tests the gRPC contract (proto/provider/v1/provider.proto) from
// the outside. It starts the Provider's real gRPC server on a loopback
// listener and talks to it with two clients: a raw providerv1.ProviderClient,
// which sees the wire exactly as sent (codes, google.rpc.ErrorInfo), and the
// manager's own transport client (internal/transport/grpc), which shows how
// the manager maps those answers (contracts.Is*). No case reaches into a
// Provider's internals.
//
// A provider is plugged in by writing one adapter: a Target. The suite never
// names a hypervisor; whatever a Target cannot offer is declared in its
// Capabilities, and the cases that need it are skipped, with the reason
// reported (Run logs every skip).
//
// Run the suite from a test:
//
//	func TestClusteredConformance(t *testing.T) {
//		clustered.Run(t, func(t clustered.T) clustered.Target { return newAdapter(t) })
//	}
package clustered

import (
	"context"
	"crypto/x509"
	"time"

	"google.golang.org/grpc"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Feature names an optional behaviour of a clustered Provider. A Target lists
// the Features it offers in Capabilities.Features; a case that needs one is
// skipped, and the skip reported, when the Target does not offer it.
type Feature string

const (
	// FeatureAsyncTasks means routed calls (Power, Delete) may return a task
	// reference that TaskStatus later reports done. A Provider whose routed
	// calls are all synchronous does not offer it.
	FeatureAsyncTasks Feature = "async-tasks"

	// FeatureVMNotOwnedReason means a refusal for a VM that exists but is
	// not the requester's carries the VM_NOT_OWNED ErrorInfo reason. The
	// contract requires only NotFound (proto DeleteRequest.owner); the reason
	// is a refinement some providers add.
	FeatureVMNotOwnedReason Feature = "vm-not-owned-reason"

	// FeatureHostInventory means the Provider serves ListHosts and
	// GetHostInfo (ADR-0007 D1, D3).
	FeatureHostInventory Feature = "host-inventory"

	// FeatureCreate means Create can be driven through the gRPC server on the
	// Target: the VM it makes can then be observed (Target.Observe) and
	// removed again.
	FeatureCreate Feature = "create"

	// FeatureCreateOwnerRebind means a repeated Create of the same owner is
	// an idempotent success and a Create of another owner for a name that is
	// taken fails with AlreadyExists (proto CreateRequest.owner).
	FeatureCreateOwnerRebind Feature = "create-owner-rebind"

	// FeatureHostBlackhole means the Target can make a host accept
	// connections but never answer (Target implements HostBlackholer).
	FeatureHostBlackhole Feature = "host-blackhole"

	// FeatureHostFaults means the Target can make a host fail one operation
	// cleanly, or lose its connection part-way through one (Target
	// implements HostFaulter).
	FeatureHostFaults Feature = "host-faults"

	// FeatureHostRemoval means the Target can remove a host from the
	// Provider's registry while VMs are bound to it (Target implements
	// HostRemover).
	FeatureHostRemoval Feature = "host-removal"

	// FeatureUnreadableStamp means SeedSpec.Stamp can make a VM's owner stamp
	// unreadable or ambiguous.
	FeatureUnreadableStamp Feature = "unreadable-stamp"

	// FeatureProviderNotReady means the Target can make the Provider itself,
	// not one host, unable to answer (Target.SetProviderReady).
	FeatureProviderNotReady Feature = "provider-not-ready"

	// FeatureOperationIdentity means the Provider reports
	// supports_operation_identity: it accepts a mutating call carried in an
	// operation_envelope and has the host verify the grant before it acts
	// (Target implements OperationIdentityTarget). The identity.* cases need
	// it; for every other Target they are skipped and listed as not
	// applicable.
	FeatureOperationIdentity Feature = "operation-identity"

	// FeatureOperationIdentityRequired means the Provider also reports
	// requires_operation_identity: it refuses a mutating call that carries no
	// operation_envelope.
	FeatureOperationIdentityRequired Feature = "operation-identity-required"

	// FeatureScriptedOutcomes means the Target can make the next operation a
	// host accepts end with a chosen typed TaskOutcome (Target implements
	// OutcomeScripter).
	FeatureScriptedOutcomes Feature = "scripted-outcomes"

	// FeatureHostAnswers means the Target's hosts answer a TaskStatus or
	// Describe that carries answer_nonce with a host-signed answer
	// (OperationAnswer, VMAnswer), keep their operation records across a
	// crash of the host's agent, and can be crashed, rolled back and made
	// to stall a step (Target implements HostAnswerTarget). The
	// identity.* cases about host answers need it; for every other Target
	// they are skipped and listed as not applicable.
	FeatureHostAnswers Feature = "host-answers"
)

// Capabilities declares what a Target offers beyond the required core.
type Capabilities struct {
	// Features are the optional behaviours the Target offers.
	Features []Feature

	// Reasons explains, per Feature the Target does NOT offer, why not. The
	// suite includes the text in the skip it reports. A missing reason is
	// reported as "not declared by the target".
	Reasons map[Feature]string
}

// Has reports whether f is offered.
func (c Capabilities) Has(f Feature) bool {
	for _, have := range c.Features {
		if have == f {
			return true
		}
	}
	return false
}

// Level says how binding a case is.
type Level int

const (
	// Required cases check a rule the proto, the contracts package or an ADR
	// states. A Target that fails one does not conform; the run fails.
	Required Level = iota
	// Recommended cases check good practice the contract does not state. A
	// Target that fails one is reported in the run's summary, and the run
	// does not fail.
	Recommended
)

// String returns the level's name.
func (l Level) String() string {
	if l == Recommended {
		return "recommended"
	}
	return "required"
}

// Identity is the owner of a VM: the VirtualMachine it was made for.
type Identity struct {
	UID       string
	Namespace string
	Name      string
}

// Proto returns the wire form of i; the zero Identity is nil, the "no owner"
// request.
func (i Identity) Proto() *providerv1.ObjectIdentity {
	if i == (Identity{}) {
		return nil
	}
	return &providerv1.ObjectIdentity{Uid: i.UID, Namespace: i.Namespace, Name: i.Name}
}

// SeedSpec describes a VM a Target places on a host out of band (not through
// Create).
type SeedSpec struct {
	// Host is the host the VM is placed on.
	Host string
	// Name is the VM's name; the Target decides the ID the Provider uses for
	// it (VM.ID).
	Name string
	// Owner is the owner stamp the VM carries. The zero Identity means the VM
	// is unstamped (made by hand, or before owner stamps existed).
	Owner Identity
	// Running is the initial power state.
	Running bool
	// Stamp makes the owner stamp unreadable or ambiguous. Only a Target that
	// offers FeatureUnreadableStamp is asked for anything but StampNormal.
	Stamp StampState
}

// StampState is the condition of a seeded VM's owner stamp.
type StampState int

const (
	// StampNormal is a readable stamp (or none, when Owner is zero).
	StampNormal StampState = iota
	// StampUnreadable is a stamp that cannot be read.
	StampUnreadable
	// StampAmbiguous is a stamp that records more than one owner.
	StampAmbiguous
)

// VM identifies a seeded or created VM in later calls.
type VM struct {
	// ID is the id the Provider uses in requests (DescribeRequest.id ...).
	ID string
	// Host is the host the VM lives on.
	Host string
	// Name is the name it was seeded or created with.
	Name string
}

// Observation is the ground truth about a VM, read from the Target's hosts
// and not through the Provider. It is how a case proves that a refused call
// changed nothing.
type Observation struct {
	// Exists reports whether a VM with that ID is on the host.
	Exists bool
	// Running reports whether it is powered on.
	Running bool
	// OwnerUID is the uid its owner stamp records ("" when unstamped).
	OwnerUID string
	// CPU is its vCPU count (0 when the Target cannot tell).
	CPU int32
	// MemoryMiB is its memory in MiB (0 when the Target cannot tell).
	MemoryMiB int64
}

// Target is the adapter between the suite and one clustered Provider. All
// methods are called from the goroutine that runs the case; a Target need not
// be safe for concurrent use except that Server's service is called by the
// gRPC server's goroutines while a case runs.
type Target interface {
	// Name labels the Target in reports.
	Name() string

	// Capabilities declares the optional behaviours the Target offers.
	Capabilities() Capabilities

	// Hosts returns the ids of the hosts the Provider fronts. At least two:
	// the suite needs a second host to prove routing and isolation.
	Hosts() []string

	// Server returns the Provider's gRPC service as the real provider binary
	// serves it. The suite registers it on its own gRPC server. After Restart
	// it returns the new service.
	Server() providerv1.ProviderServer

	// SeedVM places a VM on a host, bypassing the Provider, and returns the
	// handle to use in requests.
	SeedVM(ctx context.Context, spec SeedSpec) (VM, error)

	// Observe reads the ground truth about vm from its host.
	Observe(ctx context.Context, vm VM) (Observation, error)

	// SetHostReachable makes a host reachable or unreachable for the
	// Provider, as a network partition or a dead host would. The Provider
	// stays up; only that host stops answering.
	SetHostReachable(host string, reachable bool) error

	// Restart simulates a restart of the Provider process: everything the
	// Provider holds in memory is lost and it comes up again from its
	// configuration alone. The state of the hosts (VMs, owner stamps, host
	// reachability) is kept. Afterwards Server returns the new service.
	Restart() error
}

// GRPCServerTarget is implemented by a Target whose Provider registers its
// own gRPC service description (for example one that reads its requests as
// raw bytes) instead of the generated one. The suite serves the server it
// returns in place of a plain grpc.Server with Server() registered; it is
// called for every start of the Provider.
type GRPCServerTarget interface {
	// NewGRPCServer returns a new, not yet serving, gRPC server with the
	// Provider registered on it.
	NewGRPCServer() *grpc.Server
}

// CreateTemplater is implemented by a Target whose Create needs more than a
// name, a host and an owner (a class, an image, networks). The suite fills
// the rest of its CreateRequest from the template.
type CreateTemplater interface {
	// CreateTemplate returns the request fields the Target needs on every
	// Create: ClassJson, ImageJson, NetworksJson, DisksJson ...
	CreateTemplate() *providerv1.CreateRequest
}

// HostBlackholer is implemented by a Target that offers FeatureHostBlackhole.
type HostBlackholer interface {
	// SetHostBlackholed makes a host accept connections and never answer
	// (true), or answer again (false).
	SetHostBlackholed(host string, blackholed bool) error
}

// HostFaulter is implemented by a Target that offers FeatureHostFaults.
type HostFaulter interface {
	// FailHostOperations makes every operation that changes a VM on the host
	// fail cleanly (the host answers with an error) while on is true.
	FailHostOperations(host string, on bool) error
	// DropHostDuringOperations makes the host's connection die while it runs
	// an operation that changes a VM, after earlier reads were answered, while
	// on is true.
	DropHostDuringOperations(host string, on bool) error
}

// HostRemover is implemented by a Target that offers FeatureHostRemoval.
type HostRemover interface {
	// RemoveHost removes a host from the Provider's registry (as deleting its
	// Host would) while its VMs stay where they are.
	RemoveHost(host string) error
}

// TaskController is implemented by a Target that offers FeatureAsyncTasks.
type TaskController interface {
	// FinishTasks completes every task the Provider has issued, as the
	// hosts finishing their asynchronous operations would.
	FinishTasks() error
}

// DialObserver is implemented by a Target that can report the hosts the
// Provider tried to connect to. When it is, the suite also proves that a
// refused host id is never dialed.
type DialObserver interface {
	// DialedHosts returns every host id the Provider has connected to since
	// the Target was created or the last Restart, in order.
	DialedHosts() []string
}

// ProviderReadiness is implemented by a Target that offers
// FeatureProviderNotReady.
type ProviderReadiness interface {
	// SetProviderReady makes the Provider itself able (true) or unable (false)
	// to serve, independent of any host.
	SetProviderReady(ready bool) error
}

// OperationIdentityConfig is what the suite needs to send a Target signed
// operations: a signer whose grants chain to the anchor the Provider's hosts
// pinned, and the identity those hosts expect in a grant.
type OperationIdentityConfig struct {
	// Signer signs the grants the suite sends. Its certificate chain must
	// verify to the anchor the hosts pinned. A Target never gets a signer
	// from a release build: the suite's own tests supply a test-only one.
	Signer operation.Signer
	// ProviderUID is the provider_uid the hosts expect.
	ProviderUID string
	// Enrolments maps each host id to the 16-byte enrolment_id it expects.
	Enrolments map[string][]byte
	// Epoch is the epoch the suite issues grants under (an identity-only
	// value: the suite makes no ordering or fencing claim with it).
	Epoch operation.Epoch
	// EnrolmentStartEpoch is the epoch counter at which the hosts were
	// enrolled. An op_id minted before it is never accepted on its first
	// delivery. 0 means 1.
	EnrolmentStartEpoch uint64
}

// OperationIdentityTarget is implemented by a Target that offers
// FeatureOperationIdentity.
type OperationIdentityTarget interface {
	// OperationIdentity returns how to sign operations for the Target.
	OperationIdentity() OperationIdentityConfig
}

// OutcomeScripter is implemented by a Target that offers
// FeatureScriptedOutcomes.
type OutcomeScripter interface {
	// ScriptOutcome makes the next operation the host accepts end, when the
	// host finishes it, with outcome and outcome_reason reason instead of
	// TASK_OUTCOME_SUCCEEDED. Only RETIRED, FAILED_CLEAN, FAILED_DIRTY,
	// CANCELLED and UNKNOWN are scripted.
	ScriptOutcome(host string, outcome providerv1.TaskOutcome, reason string) error
}

// PlacementTracker is implemented by a Target whose hosts keep, per VM, the
// placement binding an operation must name and the highest attempt accepted
// per VM and kind (a per-host agent). NewOp then takes the binding and the
// next attempt from it, as a manager does, instead of drawing a fresh binding
// and attempt 1. A Create starts a new placement.
type PlacementTracker interface {
	// NextPlacement returns the binding and the next attempt for an
	// operation of kind on vmUID's placement on host.
	NextPlacement(host, vmUID string, kind providerv1.OperationKind) (binding []byte, attempt uint64)
}

// HostAnswerTarget is implemented by a Target that offers FeatureHostAnswers.
type HostAnswerTarget interface {
	// HostAnswerRoots returns the issuer certificates the hosts' signed
	// answers must chain to.
	HostAnswerRoots() *x509.CertPool
	// CrashHost kills the host's agent abruptly (as SIGKILL would) and
	// starts it again on its kept state.
	CrashHost(host string) error
	// SnapshotHost keeps a copy of the host's operation store as it is now.
	SnapshotHost(host string) error
	// RollBackHost puts the copy SnapshotHost kept back in place of the
	// host's operation store, without its anchor log (a restored backup),
	// and restarts the host's agent.
	RollBackHost(host string) error
	// StallNextStep makes the next operation step on host do nothing and
	// never return on its own: only the host's step deadline ends it.
	StallNextStep(host string) error
	// StepDeadline is the hosts' step deadline.
	StepDeadline() time.Duration
}

// identityProbe is the operation-identity probe: a case that needs
// FeatureOperationIdentity runs only against a Target that declares the
// feature and implements OperationIdentityTarget. It returns why c does not
// apply to tg, or "". The identity.capabilities case then checks that the
// Provider reports supports_operation_identity on the wire.
func identityProbe(c Case, tg Target) string {
	needs, answers := false, false
	for _, f := range c.Requires {
		switch f {
		case FeatureOperationIdentity:
			needs = true
		case FeatureHostAnswers:
			answers = true
		}
	}
	if needs {
		if _, ok := tg.(OperationIdentityTarget); !ok {
			return "the target declares operation identity but does not implement OperationIdentityTarget"
		}
	}
	if answers {
		if _, ok := tg.(HostAnswerTarget); !ok {
			return "the target declares host answers but does not implement HostAnswerTarget"
		}
	}
	return ""
}
