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

package fake

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
)

// Target is the conformance adapter of the fake clustered Provider. It
// implements clustered.Target and the optional TaskController, DialObserver
// and ProviderReadiness interfaces.
type Target struct {
	w          *world
	p          *provider
	violations map[Violation]bool
	async      bool
	caps       clustered.Capabilities
	ident      *identityMode
	placements *clustered.Placements
}

// Option configures New.
type Option func(*Target)

// WithViolations makes the fake break the given rules.
func WithViolations(vs ...Violation) Option {
	return func(t *Target) {
		for _, v := range vs {
			t.violations[v] = true
		}
	}
}

// Synchronous makes the fake's routed calls synchronous: they return no task.
func Synchronous() Option { return func(t *Target) { t.async = false } }

// New returns a fake clustered Provider over hosts host-a and host-b.
func New(opts ...Option) *Target {
	t, _ := build(nil, opts) // without identity, build never fails
	return t
}

// NewIdentity returns a fake clustered Provider over hosts host-a and host-b
// in operation-identity mode (see IdentityConfig).
func NewIdentity(cfg IdentityConfig, opts ...Option) (*Target, error) {
	return build(&cfg, opts)
}

// build makes the fake, in identity mode when identity is set.
func build(identity *IdentityConfig, opts []Option) (*Target, error) {
	t := &Target{violations: map[Violation]bool{}, async: true, placements: clustered.NewPlacements()}
	for _, o := range opts {
		o(t)
	}
	t.w = &world{hosts: map[string]*hostState{}, ready: true}
	for _, h := range []string{"host-a", "host-b"} {
		t.w.order = append(t.w.order, h)
		t.w.hosts[h] = &hostState{reachable: true, vms: map[string]*vm{}, tasks: map[string]*task{}, ops: map[string]*opRecord{}}
	}
	if identity != nil {
		m, err := newIdentityMode(*identity, t.w.order)
		if err != nil {
			return nil, err
		}
		t.ident = m
		for _, hs := range t.w.hosts {
			hs.answers = newAnswerState()
		}
	}
	t.p = t.newProvider()
	feats := []clustered.Feature{
		clustered.FeatureVMNotOwnedReason, clustered.FeatureHostInventory, clustered.FeatureCreate,
		clustered.FeatureCreateOwnerRebind, clustered.FeatureProviderNotReady, clustered.FeatureHostBlackhole,
		clustered.FeatureHostFaults, clustered.FeatureHostRemoval, clustered.FeatureUnreadableStamp,
	}
	reasons := map[clustered.Feature]string{}
	if t.async {
		feats = append(feats, clustered.FeatureAsyncTasks)
	} else {
		reasons[clustered.FeatureAsyncTasks] = "the fake is configured with synchronous calls"
	}
	if t.ident != nil {
		feats = append(feats, clustered.FeatureOperationIdentity, clustered.FeatureScriptedOutcomes, clustered.FeatureHostAnswers)
		if t.ident.required {
			feats = append(feats, clustered.FeatureOperationIdentityRequired)
		} else {
			reasons[clustered.FeatureOperationIdentityRequired] = "the fake is configured to accept unsigned calls too"
		}
	} else {
		why := "the fake is configured without operation identity"
		reasons[clustered.FeatureOperationIdentity] = why
		reasons[clustered.FeatureOperationIdentityRequired] = why
		reasons[clustered.FeatureScriptedOutcomes] = why
		reasons[clustered.FeatureHostAnswers] = why
	}
	t.caps = clustered.Capabilities{Features: feats, Reasons: reasons}
	return t, nil
}

func (t *Target) newProvider() *provider {
	return &provider{
		w: t.w, violations: t.violations, async: t.async, notOwned: true,
		memTasks: map[string]*task{}, ident: t.ident, memOps: map[string]map[string]*opRecord{},
		placements: t.placements,
	}
}

// Name labels the fake in reports.
func (t *Target) Name() string { return "fake-clustered" }

// Capabilities declares what the fake offers.
func (t *Target) Capabilities() clustered.Capabilities { return t.caps }

// Hosts returns the fake's host ids.
func (t *Target) Hosts() []string { return append([]string(nil), t.w.order...) }

// Server returns the fake's gRPC service.
func (t *Target) Server() providerv1.ProviderServer {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	t.p.served++
	if t.violations[ForgetsVMsOnRestart] && t.p.served > 1 {
		t.wipeVMs()
	}
	return t.p
}

func (t *Target) wipeVMs() {
	for _, hs := range t.w.hosts {
		hs.vms = map[string]*vm{}
	}
}

// RemoveVM removes the VM id from host behind the provider's back (as if it
// vanished on the hypervisor), without any record of a Delete.
func (t *Target) RemoveVM(host, id string) error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	hs := t.w.hosts[host]
	if hs == nil {
		return fmt.Errorf("fake: no host %q", host)
	}
	if _, ok := hs.vms[id]; !ok {
		return fmt.Errorf("fake: no VM %q on host %q", id, host)
	}
	delete(hs.vms, id)
	return nil
}

// SeedVM places a VM on a host.
func (t *Target) SeedVM(_ context.Context, spec clustered.SeedSpec) (clustered.VM, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	hs := t.w.hosts[spec.Host]
	if hs == nil {
		return clustered.VM{}, fmt.Errorf("fake: no host %q", spec.Host)
	}
	hs.vms[spec.Name] = &vm{name: spec.Name, owner: spec.Owner, running: spec.Running, cpu: 2, memMiB: 2048, stamp: spec.Stamp}
	return clustered.VM{ID: spec.Name, Host: spec.Host, Name: spec.Name}, nil
}

// Observe reads the ground truth.
func (t *Target) Observe(_ context.Context, v clustered.VM) (clustered.Observation, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	hs := t.w.hosts[v.Host]
	if hs == nil {
		return clustered.Observation{}, fmt.Errorf("fake: no host %q", v.Host)
	}
	x := hs.vms[v.ID]
	if x == nil {
		return clustered.Observation{}, nil
	}
	return clustered.Observation{Exists: true, Running: x.running, OwnerUID: x.owner.UID, CPU: x.cpu, MemoryMiB: x.memMiB}, nil
}

// SetHostReachable makes a host reachable or not.
func (t *Target) SetHostReachable(host string, reachable bool) error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	hs := t.w.hosts[host]
	if hs == nil {
		return fmt.Errorf("fake: no host %q", host)
	}
	hs.reachable = reachable
	return nil
}

// Restart simulates a restart of the provider process.
func (t *Target) Restart() error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	t.p = t.newProvider()
	t.w.dialed = nil
	for _, hs := range t.w.hosts {
		hs.stuck = false
	}
	if t.violations[ForgetsVMsOnRestart] {
		t.wipeVMs()
	}
	if t.violations[RestoresDeletedOnRestart] {
		for _, d := range t.w.deleted {
			v := d.v
			t.w.hosts[d.host].vms[v.name] = &v
		}
	}
	if t.violations[RestartDropsDownHost] {
		for i, h := range t.w.order {
			if !t.w.hosts[h].reachable {
				t.w.order = append(t.w.order[:i:i], t.w.order[i+1:]...)
				delete(t.w.hosts, h)
				break
			}
		}
	}
	return nil
}

// FinishTasks completes every task the provider has issued.
func (t *Target) FinishTasks() error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	finish := func(k *task) {
		if k.done || k.stalled {
			return
		}
		k.done = true
		if k.typed && !appliesEffect(k.outcome) {
			return
		}
		if !t.violations[TaskDoneWithoutEffect] {
			k.apply()
		}
	}
	for _, hs := range t.w.hosts {
		for _, k := range hs.tasks {
			finish(k)
		}
	}
	for _, k := range t.p.memTasks {
		finish(k)
	}
	if t.violations[DropsTombstones] {
		for _, hs := range t.w.hosts {
			for id, rec := range hs.ops {
				if k := hs.tasks[rec.task]; k != nil && k.done {
					delete(hs.ops, id)
				}
			}
		}
	}
	return nil
}

// DialedHosts returns the host ids the provider connected to.
func (t *Target) DialedHosts() []string {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	return append([]string(nil), t.w.dialed...)
}

// SetProviderReady makes the provider able or unable to serve.
func (t *Target) SetProviderReady(ready bool) error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	t.w.ready = ready
	return nil
}

// SetHostBlackholed makes a host accept connections and never answer.
func (t *Target) SetHostBlackholed(host string, blackholed bool) error {
	return t.withHost(host, func(hs *hostState) { hs.blackholed = blackholed })
}

// FailHostOperations makes operations that change a VM fail on the host.
func (t *Target) FailHostOperations(host string, on bool) error {
	return t.withHost(host, func(hs *hostState) { hs.failOps = on })
}

// DropHostDuringOperations makes the host's connection die during operations
// that change a VM.
func (t *Target) DropHostDuringOperations(host string, on bool) error {
	return t.withHost(host, func(hs *hostState) { hs.dropOps = on })
}

// RemoveHost removes a host from the provider's registry; its VMs stay.
func (t *Target) RemoveHost(host string) error {
	return t.withHost(host, func(hs *hostState) { hs.removed = true })
}

func (t *Target) withHost(host string, fn func(*hostState)) error {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	hs := t.w.hosts[host]
	if hs == nil {
		return fmt.Errorf("fake: no host %q", host)
	}
	fn(hs)
	return nil
}

// NextPlacement implements clustered.PlacementTracker: the fake keeps each
// VM's placement binding and attempts per kind, as a per-host agent does.
func (t *Target) NextPlacement(host, vmUID string, kind providerv1.OperationKind) ([]byte, uint64) {
	return t.placements.NextPlacement(host, vmUID, kind)
}

// HostAnswerRoots implements clustered.HostAnswerTarget.
func (t *Target) HostAnswerRoots() *x509.CertPool {
	if t.ident == nil {
		return x509.NewCertPool()
	}
	return t.ident.hostKeys.roots
}

// CrashHost implements clustered.HostAnswerTarget: the host's records are on
// the host and survive (unless the fake commits ForgetsOpsOnHostCrash).
func (t *Target) CrashHost(host string) error {
	return t.withHost(host, func(hs *hostState) {
		if t.violations[ForgetsOpsOnHostCrash] {
			hs.ops = map[string]*opRecord{}
		}
	})
}

// SnapshotHost implements clustered.HostAnswerTarget. The fake has no store
// to copy: a later RollBackHost quarantines the host.
func (t *Target) SnapshotHost(host string) error { return t.withHost(host, func(*hostState) {}) }

// RollBackHost implements clustered.HostAnswerTarget: the host's store is
// quarantined (rolled_back).
func (t *Target) RollBackHost(host string) error {
	return t.withHost(host, func(hs *hostState) {
		if hs.answers != nil {
			hs.answers.quarantine = rolledBackReason
		}
	})
}

// StallNextStep implements clustered.HostAnswerTarget.
func (t *Target) StallNextStep(host string) error {
	return t.withHost(host, func(hs *hostState) {
		if hs.answers != nil {
			hs.answers.stallNext = true
		}
	})
}

// StepDeadline implements clustered.HostAnswerTarget.
func (t *Target) StepDeadline() time.Duration { return StepDeadline }
