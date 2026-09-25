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

package scheduler

import (
	"errors"
	"fmt"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ErrResizeDoesNotFit is the sentinel every *ResizeDoesNotFitError matches
// (errors.Is).
var ErrResizeDoesNotFit = errors.New("resize does not fit on the VM's host")

// ResizeRequest asks whether a VM already placed on Host may be resized from
// Current to Desired (ADR-0007 Addendum A, scheduler-accuracy amendment: a
// clustered resize-up is admitted against committed capacity). Like Request,
// it is a pure, self-contained input.
type ResizeRequest struct {
	// Host is the VM's host, with its live status.
	Host v1beta1.Host
	// Pool carries the overcommit ratios of the host's HostPool.
	Pool v1beta1.HostPoolSpec
	// VMUID identifies the VM being resized: its own PlacedVMs entries (its
	// durable record, an earlier resize assumption) are ignored, so its
	// current footprint never counts against its resize.
	VMUID string
	// Current is the size the VM holds now; Desired the size it asks for.
	Current, Desired ResourceRequest
	// PlacedVMs is what is committed on the Provider's hosts, as for Request.
	PlacedVMs []PlacedVM
}

// ResizeDoesNotFitError reports a resize-up that exceeds the free capacity of
// the VM's host. Its Error names only the VM's own sizes and host; Shortfalls
// carry the committed-capacity arithmetic (derived from other tenants' VMs)
// for the manager log.
type ResizeDoesNotFitError struct {
	// HostID is the VM's host.
	HostID string
	// Current and Desired are the VM's own sizes.
	Current, Desired ResourceRequest
	// Shortfalls holds one entry per growing resource that does not fit, with
	// Reason rejInsufficientCPU or rejInsufficientMem.
	Shortfalls []HostRejection
}

// Error implements error. It carries no committed, capacity or free figure.
func (e *ResizeDoesNotFitError) Error() string {
	return fmt.Sprintf("resizing to %d vCPU and %d MiB exceeds the free capacity of its host %s",
		e.Desired.CPU, e.Desired.MemoryMiB, e.HostID)
}

// Is makes errors.Is(err, ErrResizeDoesNotFit) true.
func (e *ResizeDoesNotFitError) Is(target error) bool { return target == ErrResizeDoesNotFit }

// Detail renders the per-resource arithmetic for the manager log, e.g.
// "insufficient CPU capacity: requested 4, free 2 (committed 14 of 16)".
func (e *ResizeDoesNotFitError) Detail() []string {
	out := make([]string, 0, len(e.Shortfalls))
	for _, s := range e.Shortfalls {
		out = append(out, fmt.Sprintf("%s: %s", s.Reason, s.Shortfall))
	}
	return out
}

// Grows reports whether desired increases CPU or memory over current.
func Grows(current, desired ResourceRequest) bool {
	return desired.CPU > current.CPU || desired.MemoryMiB > current.MemoryMiB
}

// CheckResize reports whether the resize in req fits on its host. It returns
// nil when the VM does not grow (a shrink is always allowed) or when every
// growing resource fits in the host's free capacity — its effective capacity
// (allocatable × the pool's overcommit ratio) minus what the OTHER VMs commit
// there. A resource that does not grow is not checked, so shrinking memory on a
// host whose memory is already over-committed does not block growing its CPU.
// Host health and cordon are not checked: a resize does not move the VM.
//
// It returns a *ResizeDoesNotFitError (matching ErrResizeDoesNotFit) when a
// growing resource does not fit, and an ordinary error for a malformed pool
// overcommit ratio.
func CheckResize(req ResizeRequest) error {
	if !Grows(req.Current, req.Desired) {
		return nil
	}
	ec, err := newEvalContext(Request{Pool: req.Pool, PlacedVMs: req.PlacedVMs, VMUID: req.VMUID, Resources: req.Desired})
	if err != nil {
		return err
	}
	var shortfalls []HostRejection
	if req.Desired.CPU > req.Current.CPU {
		if free, _ := ec.freeCapacity(&req.Host); free < int64(req.Desired.CPU) {
			shortfalls = append(shortfalls, HostRejection{HostID: req.Host.Name, Reason: rejInsufficientCPU,
				Shortfall: ec.capacityShortfall(&req.Host, rejInsufficientCPU)})
		}
	}
	if req.Desired.MemoryMiB > req.Current.MemoryMiB {
		if _, free := ec.freeCapacity(&req.Host); free < req.Desired.MemoryMiB {
			shortfalls = append(shortfalls, HostRejection{HostID: req.Host.Name, Reason: rejInsufficientMem,
				Shortfall: ec.capacityShortfall(&req.Host, rejInsufficientMem)})
		}
	}
	if len(shortfalls) == 0 {
		return nil
	}
	return &ResizeDoesNotFitError{HostID: req.Host.Name, Current: req.Current, Desired: req.Desired, Shortfalls: shortfalls}
}
