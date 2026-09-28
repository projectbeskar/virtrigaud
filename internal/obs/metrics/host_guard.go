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

package metrics

import "github.com/prometheus/client_golang/prometheus"

// Reasons of virtrigaud_provider_host_guard_refusals_total: which check of the
// host guard refused a long routed host command (ADR-0007 Addendum A, slice
// 3). A fixed, low-cardinality set.
const (
	// HostGuardReasonLockDirUnsafe: the provider's lock directory on the host
	// is a symbolic link, not a directory, or not owned by the SSH user (for
	// example another local user created it first).
	HostGuardReasonLockDirUnsafe = "lock_dir_unsafe"
	// HostGuardReasonLockSymlink: a lock file is a symbolic link.
	HostGuardReasonLockSymlink = "lock_symlink"
	// HostGuardReasonTargetSymlink: the file the command writes (a clone's
	// disk, an export's copy) is a symbolic link.
	HostGuardReasonTargetSymlink = "target_symlink"
	// HostGuardReasonUnknown: the guard refused without a recognized reason.
	HostGuardReasonUnknown = "unknown"
)

// hostGuardRefusalsTotal counts the long routed host commands (a clone's disk
// copy, an export's flatten) that the host guard refused to run. Every
// increment is worth an administrator's attention: it means a host path
// VirtRigaud writes to was tampered with or pre-created by another local user.
// Emitted by the provider process; labels are the provider type and the
// refusal reason only (no host, path or tenant).
var hostGuardRefusalsTotal = registerer.NewCounterVec(
	prometheus.CounterOpts{
		Name: "virtrigaud_provider_host_guard_refusals_total",
		Help: "Long routed host commands the host guard refused to run (an unsafe lock directory, or a symlinked lock or target), by provider type and reason. Any increment needs an administrator's attention.",
	},
	[]string{"provider_type", "reason"},
)

// RecordHostGuardRefusal counts one host-guard refusal.
func RecordHostGuardRefusal(providerType, reason string) {
	hostGuardRefusalsTotal.WithLabelValues(providerType, reason).Inc()
}
