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
	"fmt"

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
	t := &Target{violations: map[Violation]bool{}, async: true}
	for _, o := range opts {
		o(t)
	}
	t.w = &world{hosts: map[string]*hostState{}, ready: true}
	for _, h := range []string{"host-a", "host-b"} {
		t.w.order = append(t.w.order, h)
		t.w.hosts[h] = &hostState{reachable: true, vms: map[string]*vm{}, tasks: map[string]*task{}}
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
	t.caps = clustered.Capabilities{Features: feats, Reasons: reasons}
	return t
}

func (t *Target) newProvider() *provider {
	return &provider{
		w: t.w, violations: t.violations, async: t.async, notOwned: true,
		memTasks: map[string]*task{},
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
		if k.done {
			return
		}
		k.done = true
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
