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
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// These tests compare the allocation-efficient snapshot (C′: the index read
// directly, VMs summed per host, only assumed and label-matched VMs listed)
// with the reference accounting (placement_reference_test.go) on identical
// fixtures and identical assumption caches: same per-host committed capacity
// and affinity counts, same decisions and no-fits, same resize, clone and
// inherited-admission verdicts, and the same assumptions settled.

// equivalenceFixture is a scenario's fixture, shrunk, with every record shape
// the accounting distinguishes mixed in.
func equivalenceFixture(s schedbench.Scenario) *schedbench.Fixture {
	cfg := s.Config
	cfg.Hosts, cfg.VMs = 40, 800
	cfg.PendingFraction, cfg.UnrecordedFraction = 0.1, 0.1
	f := schedbench.Build(cfg)
	now := metav1.NewTime(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	for i := range f.VMs {
		vm := &f.VMs[i]
		pl := vm.Status.Placement
		switch {
		case i%17 == 0 && pl != nil && pl.Host != "":
			// Dual-host: bound, with a pending host elsewhere (a move).
			pl.PendingHost = f.Hosts[(i+1)%len(f.Hosts)].Name
		case i%23 == 0 && pl != nil:
			pl.ResizeReservation = &infravirtrigaudiov1beta1.PlacementResources{CPU: 6, MemoryMiB: 12288}
		case i%29 == 0:
			// Being deleted, finalizer still there: still counts.
			vm.DeletionTimestamp = &now
			vm.Finalizers = append(vm.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
		case i%31 == 0:
			// Being deleted, finalizer gone: holds nothing.
			vm.DeletionTimestamp = &now
			vm.Finalizers = []string{"other.io/keep"}
		case i%37 == 0 && pl != nil:
			ceiling := int64(32768)
			pl.MemoryCeilingMiB = &ceiling
		}
	}
	return f
}

// seedAssumptions makes the same assumptions in c for every world: creates
// on the recorded host (settle), on another host (do not), of VMs the
// snapshot does not have (seen and unseen), resizes covered and not, and
// writes in every state.
func seedAssumptions(t *testing.T, c *assume.Cache, key string, f *schedbench.Fixture) {
	t.Helper()
	host := func(i int) string { return f.Hosts[i%len(f.Hosts)].Name }
	for i := 0; i < len(f.VMs); i += 41 {
		vm := &f.VMs[i]
		a := assume.Assumption{UID: string(vm.UID), Namespace: vm.Namespace, Name: vm.Name, Labels: vm.Labels,
			Resources: scheduler.ResourceRequest{CPU: 3, MemoryMiB: 6144}, Observed: i%2 == 0}
		switch (i / 41) % 4 {
		case 0: // its record's host: settles
			a.HostID = placementHosts(vm)[0]
		case 1: // another host: does not
			a.HostID = host(i + 3)
		case 2: // an admitted resize, larger than the record
			a.HostID, a.Resize = placementHosts(vm)[0], true
			a.Resources = scheduler.ResourceRequest{CPU: 64, MemoryMiB: 262144}
		case 3: // an admitted resize the record covers
			a.HostID, a.Resize = placementHosts(vm)[0], true
			a.Resources = scheduler.ResourceRequest{CPU: 1, MemoryMiB: 128}
		}
		tok, ok := c.Assume(key, a)
		require.True(t, ok)
		switch (i / 41) % 3 {
		case 1:
			require.True(t, c.BeginWrite(a.UID, tok))
		case 2:
			require.True(t, c.BeginWrite(a.UID, tok))
			c.WriteStored(a.UID, tok)
		}
	}
	for j, observed := range []bool{true, false} {
		// A VM the snapshot does not have (new, or gone).
		_, ok := c.Assume(key, assume.Assumption{UID: fmt.Sprintf("uid-absent-%d", j), Namespace: schedbench.SelfNamespace,
			Name: fmt.Sprintf("absent-%d", j), HostID: host(j), Resources: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096},
			Observed: observed})
		require.True(t, ok)
	}
}

// hostTotals is what the scheduler commits per host from PlacedVMs and
// Committed: entries merged per (UID, host) at their larger size, self and
// host-less ones skipped, as indexPlaced does.
type hostTotals struct {
	cpu, mem int64
	scoped   int
}

func committedTotals(placed []scheduler.PlacedVM, byHost map[string]scheduler.HostCommitted, self string) map[string]hostTotals {
	type key struct{ uid, host string }
	merged := map[key]scheduler.PlacedVM{}
	var anon []scheduler.PlacedVM
	for _, p := range placed {
		if p.HostID == "" || (p.UID != "" && p.UID == self) {
			continue
		}
		if p.UID == "" {
			anon = append(anon, p)
			continue
		}
		k := key{p.UID, p.HostID}
		if m, ok := merged[k]; ok {
			m.Resources.CPU = max(m.Resources.CPU, p.Resources.CPU)
			m.Resources.MemoryMiB = max(m.Resources.MemoryMiB, p.Resources.MemoryMiB)
			merged[k] = m
			continue
		}
		merged[k] = p
	}
	out := map[string]hostTotals{}
	add := func(p scheduler.PlacedVM) {
		t := out[p.HostID]
		t.cpu = scheduler.SaturatingAdd(t.cpu, max(int64(p.Resources.CPU), 0))
		t.mem = scheduler.SaturatingAdd(t.mem, max(p.Resources.MemoryMiB, 0))
		if !p.CapacityOnly {
			t.scoped++
		}
		out[p.HostID] = t
	}
	for _, p := range merged {
		add(p)
	}
	for _, p := range anon {
		add(p)
	}
	for h, c := range byHost {
		t := out[h]
		t.cpu = scheduler.SaturatingAdd(t.cpu, c.CPU)
		t.mem = scheduler.SaturatingAdd(t.mem, c.MemoryMiB)
		t.scoped += c.ScopedVMs
		out[h] = t
	}
	return out
}

// liveAssumptions renders c's live assumptions of key, with their write
// state, sorted.
func liveAssumptions(c *assume.Cache, key string) []string {
	var out []string
	for _, a := range c.Observe(key, func(assume.Assumption) assume.Observation { return assume.Observation{Unknown: true} }) {
		_, tok, _ := c.Lookup(a.UID)
		out = append(out, fmt.Sprintf("%s@%s:%d/%d:%v:%d", a.UID, a.HostID, a.Resources.CPU, a.Resources.MemoryMiB, a.Write, tok))
	}
	slices.Sort(out)
	return out
}

func TestSnapshotEquivalence_Decisions(t *testing.T) {
	ctx := context.Background()
	seen := map[string]bool{}
	for _, s := range schedbench.Scenarios() {
		variant := s.Name[strings.Index(s.Name, "/")+1:]
		if seen[variant] {
			continue
		}
		seen[variant] = true
		t.Run(variant, func(t *testing.T) {
			f := equivalenceFixture(s)
			w := newBenchWorld(f)
			p := w.providers[0]
			clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			now := func() time.Time { return clock }
			refCache, newCache := assume.New(placementAssumeTTL, now), assume.New(placementAssumeTTL, now)
			seedAssumptions(t, refCache, p.key, f)
			seedAssumptions(t, newCache, p.key, f)
			require.Equal(t, liveAssumptions(refCache, p.key), liveAssumptions(newCache, p.key))

			probe := w.scenarioVM(p, "probe", s.Class)
			req := scenarioRequest(p, s)

			placedRef, selfRef, err := refPlacedWithAssumptions(ctx, w.r.Client, refCache, p.key, p.provider, probe)
			require.NoError(t, err)
			view, selfNew, err := placedWithAssumptionsFrom(ctx, w.r.Client, newCache, p.key, p.provider, probe,
				scheduler.NeedsPlacedVMLabels(req.Policy))
			require.NoError(t, err)
			assert.Equal(t, selfRef, selfNew, "self present")
			self := vmSchedulingUID(probe)

			// 1, 4–6: per-host committed capacity and affinity counts, with
			// pending, dual-host, reservations, ceilings, deleting VMs and
			// assumptions merged.
			assert.Equal(t, committedTotals(placedRef, nil, self), committedTotals(view.placed, view.byHost, self))
			// The same assumptions settled, nothing else touched.
			assert.Equal(t, liveAssumptions(refCache, p.key), liveAssumptions(newCache, p.key))

			// 2, 3, 6: the same host, trace and no-fit.
			reqRef, reqNew := req, req
			reqRef.PlacedVMs, reqRef.VMUID = placedRef, self
			reqNew.PlacedVMs, reqNew.Committed, reqNew.VMUID = view.placed, view.byHost, self
			resRef, errRef := scheduler.Schedule(reqRef)
			resNew, errNew := scheduler.Schedule(reqNew)
			assert.Equal(t, resRef, resNew)
			assert.Equal(t, errString(errRef), errString(errNew))
			if s.Expect == schedbench.ExpectNoFit {
				require.ErrorIs(t, errNew, scheduler.ErrNoFeasibleHost)
			}
		})
	}
}

// TestSnapshotEquivalence_ResizeCloneAndInherited checks the label-free
// checks — a resize, a clone or an inherited admission, all
// scheduler.CheckResize over placedWithAssumptionsFrom(detail false) — from
// placed VMs' own points of view (their records and reservations excluded).
func TestSnapshotEquivalence_ResizeCloneAndInherited(t *testing.T) {
	ctx := context.Background()
	s := schedbench.Scenarios()[0]
	s.Config.Utilization = 0.85
	f := equivalenceFixture(s)
	w := newBenchWorld(f)
	p := w.providers[0]
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	refCache, newCache := assume.New(placementAssumeTTL, now), assume.New(placementAssumeTTL, now)
	seedAssumptions(t, refCache, p.key, f)
	seedAssumptions(t, newCache, p.key, f)
	hostByName := map[string]infravirtrigaudiov1beta1.Host{}
	for _, h := range f.Hosts {
		hostByName[h.Name] = h
	}
	checked := 0
	for i := 3; i < len(f.VMs); i += 13 {
		vm := &f.VMs[i]
		hosts := placementHosts(vm)
		if len(hosts) == 0 {
			continue
		}
		placedRef, _, err := refPlacedWithAssumptions(ctx, w.r.Client, refCache, p.key, p.provider, vm)
		require.NoError(t, err)
		view, _, err := placedWithAssumptionsFrom(ctx, w.r.Client, newCache, p.key, p.provider, vm, false)
		require.NoError(t, err)
		self := vmSchedulingUID(vm)
		require.Equal(t, committedTotals(placedRef, nil, self), committedTotals(view.placed, view.byHost, self), "VM %s", vm.Name)
		for _, desired := range []scheduler.ResourceRequest{{CPU: 2, MemoryMiB: 4096}, {CPU: 16, MemoryMiB: 65536}, {CPU: 64, MemoryMiB: 524288}} {
			for _, current := range []scheduler.ResourceRequest{{}, {CPU: 2, MemoryMiB: 2048}} {
				base := scheduler.ResizeRequest{Host: hostByName[hosts[0]], Pool: f.Pool, VMUID: self, Current: current, Desired: desired}
				ref, nw := base, base
				ref.PlacedVMs = placedRef
				nw.PlacedVMs, nw.Committed = view.placed, view.byHost
				errRef, errNew := scheduler.CheckResize(ref), scheduler.CheckResize(nw)
				assert.Equal(t, errString(errRef), errString(errNew))
				var dRef, dNew *scheduler.ResizeDoesNotFitError
				if assert.Equal(t, asResizeErr(errRef, &dRef), asResizeErr(errNew, &dNew)) && dRef != nil {
					assert.Equal(t, dRef.Detail(), dNew.Detail(), "the arithmetic too")
				}
				checked++
			}
		}
	}
	require.Greater(t, checked, 100)
	assert.Equal(t, liveAssumptions(refCache, p.key), liveAssumptions(newCache, p.key))
}

// asResizeErr reports whether err is a *scheduler.ResizeDoesNotFitError.
func asResizeErr(err error, target **scheduler.ResizeDoesNotFitError) bool {
	return err != nil && errors.As(err, target)
}

// TestSnapshot_SummedVMsNeverMeetLabelMatching: the scheduler refuses summed
// VMs in the affinity scope when the policy matches VM labels (the caller's
// contract), and the controller lists them in that case.
func TestSnapshot_SummedVMsNeverMeetLabelMatching(t *testing.T) {
	var affinity schedbench.Scenario
	for _, s := range schedbench.Scenarios() {
		if strings.HasSuffix(s.Name, "/affinity") {
			affinity = s
			break
		}
	}
	f := equivalenceFixture(affinity)
	w := newBenchWorld(f)
	p := w.providers[0]
	probe := w.scenarioVM(p, "probe", affinity.Class)
	req := scenarioRequest(p, affinity)
	require.True(t, scheduler.NeedsPlacedVMLabels(req.Policy))

	view, _, err := placedWithAssumptionsFrom(context.Background(), w.r.Client, assume.New(time.Minute, nil), p.key, p.provider, probe, true)
	require.NoError(t, err)
	for h, c := range view.byHost {
		assert.Zero(t, c.ScopedVMs, "host %s: a VM in the affinity scope was summed", h)
	}

	bad := req
	bad.Committed = map[string]scheduler.HostCommitted{f.Hosts[0].Name: {CPU: 1, MemoryMiB: 1, VMs: 1, ScopedVMs: 1}}
	_, err = scheduler.Schedule(bad)
	require.Error(t, err)
	assert.NotErrorIs(t, err, scheduler.ErrNoFeasibleHost)
}
