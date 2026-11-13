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

// Package executor defines how the per-host agent's operation store drives
// the component that performs an operation's effects on the host.
//
// This package holds the interface only. The store calls an Executor for each
// step of an operation, in order, after the step's intent is durable; it
// never calls one before the store's continuity is established and its
// recovery has run.
//
// Every step has a class:
//
//   - idempotent (neither Once nor Irreversible): re-running it is safe;
//   - Once: its effect must happen at most once. The store creates the
//     step's witness file, durably, before calling Run, and after a crash it
//     asks Inspect, by the operation's launch identity, whether the effect
//     exists; it never runs the step again;
//   - Irreversible: its effect cannot be undone (data loss, a guest-visible
//     effect). After a crash the store asks Goal whether the step's goal
//     state holds; it never runs the step again.
//
// A step may be both Once and Irreversible.
package executor

import (
	"context"
	"errors"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Step is one step of an operation's plan.
type Step struct {
	// Name identifies the step within its plan. Names are unique in a plan.
	Name string
	// Once marks a step whose effect must happen at most once.
	Once bool
	// Irreversible marks a step whose effect cannot be undone.
	Irreversible bool
}

// Operation identifies the operation a step belongs to. It carries no
// request bytes except Payload, which the store never writes anywhere.
type Operation struct {
	// OpID is the operation's 16-byte identifier. It is the launch identity
	// an executor stamps on everything it creates for the operation, so that
	// Inspect can find the operation's effects.
	OpID []byte
	// VMUID is the VirtualMachine metadata.uid.
	VMUID string
	// BindingID is the VM's placement identifier on this host.
	BindingID []byte
	// Kind is the operation kind.
	Kind providerv1.OperationKind
	// Attempt is the attempt number.
	Attempt uint64
	// Payload is the verified request, held in memory only for the duration
	// of the call. It is nil when the store resumes an operation after a
	// restart.
	Payload []byte
}

// Evidence is what an inspection found.
type Evidence int

const (
	// EvidenceUnknown: the inspection could not decide. The store never
	// guesses from it.
	EvidenceUnknown Evidence = iota
	// EvidencePresent: the step's effect for this operation exists.
	EvidencePresent
	// EvidenceAbsent: the step's effect for this operation positively does
	// not exist.
	EvidenceAbsent
)

// ErrNotPerformed is returned (wrapped) by Run when the step definitely did
// not perform its effect. Any other error leaves the effect in doubt.
var ErrNotPerformed = errors.New("step not performed")

// ErrUnsupportedKind is returned (wrapped) by Plan for a kind the executor
// does not implement.
var ErrUnsupportedKind = errors.New("operation kind not supported by this executor")

// Executor performs operation steps on the host.
//
// Implementations must be safe for concurrent use for different operations.
// The store never calls Run twice concurrently for the same operation.
type Executor interface {
	// Plan returns the steps of an operation of kind, in order. It returns
	// the same plan for the same kind for the life of an enrolment.
	Plan(kind providerv1.OperationKind) ([]Step, error)
	// Run performs step of op. It returns nil only when the effect is done.
	Run(ctx context.Context, op Operation, step Step) error
	// Inspect reports, by op's launch identity, whether step's effect for op
	// exists. It is asked about Once steps whose witness exists, and about
	// steps whose Run did not finish cleanly.
	Inspect(ctx context.Context, op Operation, step Step) (Evidence, error)
	// Goal reports whether the goal state of an Irreversible step holds for
	// op (for example: the file is absent, the process positively absent).
	Goal(ctx context.Context, op Operation, step Step) (bool, error)
}

// Stopper stops every VM an executor runs. The agent's shutdown command calls
// it only while the host itself is shutting down, rebooting, halting or
// powering off.
type Stopper interface {
	// StopAll stops every VM. It returns nil when nothing runs.
	StopAll(ctx context.Context) error
}
