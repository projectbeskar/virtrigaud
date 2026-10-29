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

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// hostsFile is the host-inventory Secret the Provider controller renders and
// mounts (ADR-0007 D3); the fake serves exactly the hosts it lists.
var hostsFile = filepath.Join(hostsecret.MountPath, hostsecret.SecretDataKey)

// inventoryReload is how often the inventory file is re-read.
const inventoryReload = 5 * time.Second

// vmRecord is one VM executing (or stopped) on one simulated host. It is the
// fake agent's view, independent of anything the manager wrote to the API.
type vmRecord struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	OwnerUID  string    `json:"ownerUID"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	CPU       int32     `json:"cpu"`
	MemoryMiB int64     `json:"memoryMiB"`
	Power     string    `json:"power"`
	IP        string    `json:"ip"`
	Seeded    bool      `json:"seeded"`
	Created   time.Time `json:"created"`
}

// hostAgent is one simulated host agent: its VMs and its execution slots.
type hostAgent struct {
	mu  sync.Mutex
	vms map[string]*vmRecord
	// slots bounds concurrent executions on the host (create, delete, power).
	slots chan struct{}
}

// state is the fake cluster: the hosts the inventory names and their VMs.
type state struct {
	mu    sync.RWMutex
	hosts map[string]*hostAgent
	// byOwner indexes records by owner namespace/name (ListVMs owner filter).
	byOwner map[string][]*vmRecord
	// running counts, per owner UID, the hosts it runs on (duplicate
	// execution detection).
	running map[string]map[string]bool
	cfg     func() *config
	m       *fakeMetrics
	log     *slog.Logger
}

// newState returns an empty fake cluster.
func newState(cfg func() *config, m *fakeMetrics, log *slog.Logger) *state {
	return &state{
		hosts:   map[string]*hostAgent{},
		byOwner: map[string][]*vmRecord{},
		running: map[string]map[string]bool{},
		cfg:     cfg,
		m:       m,
		log:     log,
	}
}

// ownerKey is the byOwner key.
func ownerKey(ns, name string) string { return ns + "/" + name }

// syncInventory reads the inventory file and adds the hosts it routes. A
// host that leaves the inventory keeps its VMs (a removed Host CR does not
// stop a running VM) but is no longer served.
func (s *state) syncInventory() {
	data, err := os.ReadFile(hostsFile)
	if err != nil {
		s.log.Debug("inventory not readable", "path", hostsFile, "error", err)
		return
	}
	inv, err := hostsecret.Unmarshal(data)
	if err != nil {
		s.log.Warn("inventory does not parse", "error", err)
		return
	}
	served := map[string]bool{}
	for _, h := range inv.Hosts {
		served[h.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conc := max(s.cfg().HostConcurrency, 1)
	for id := range served {
		if _, ok := s.hosts[id]; !ok {
			s.hosts[id] = &hostAgent{vms: map[string]*vmRecord{}, slots: make(chan struct{}, conc)}
		}
	}
	s.m.hosts.Set(float64(len(served)))
}

// watchInventory re-reads the inventory until ctx ends.
func (s *state) watchInventory(ctx context.Context) {
	s.syncInventory()
	t := time.NewTicker(inventoryReload)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncInventory()
		}
	}
}

// seed builds the execution state of a seeded population. Seeded VMs have
// no owner UID until the first owner-bearing call adopts one: the driver
// creates their VirtualMachines (and so their UIDs) independently.
func (s *state) seed(spec workload.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conc := max(s.cfg().HostConcurrency, 1)
	now := time.Now()
	for i := 0; i < spec.Count; i++ {
		v := spec.VM(i)
		h, ok := s.hosts[v.Host]
		if !ok {
			h = &hostAgent{vms: map[string]*vmRecord{}, slots: make(chan struct{}, conc)}
			s.hosts[v.Host] = h
		}
		rec := &vmRecord{
			ID: v.ID, Host: v.Host, Namespace: v.Namespace, Name: v.Name,
			CPU: v.Class.CPU, MemoryMiB: v.Class.MemoryMiB, Power: powerOn, IP: v.IP,
			Seeded: true, Created: now,
		}
		h.vms[v.ID] = rec
		k := ownerKey(v.Namespace, v.Name)
		s.byOwner[k] = append(s.byOwner[k], rec)
	}
	s.m.vms.Add(float64(spec.Count))
}

// host returns the agent of a served host, or nil.
func (s *state) host(id string) *hostAgent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hosts[id]
}

// unreachable reports whether host is configured unreachable.
func (s *state) unreachable(host string) bool {
	for _, h := range s.cfg().UnreachableHosts {
		if h == host {
			return true
		}
	}
	return false
}

// addRecord indexes a new record and checks duplicate execution: the same
// owner UID running on a second host is a violation.
func (s *state) addRecord(rec *vmRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ownerKey(rec.Namespace, rec.Name)
	s.byOwner[k] = append(s.byOwner[k], rec)
	s.markRunningLocked(rec)
	s.m.vms.Inc()
}

// markRunningLocked records rec's owner as running on its host.
func (s *state) markRunningLocked(rec *vmRecord) {
	if rec.OwnerUID == "" {
		return
	}
	hosts := s.running[rec.OwnerUID]
	if hosts == nil {
		hosts = map[string]bool{}
		s.running[rec.OwnerUID] = hosts
	}
	hosts[rec.Host] = true
	if len(hosts) > 1 {
		s.m.duplicateIdentity.Inc()
		s.log.Error("DUPLICATE EXECUTION: owner runs on more than one host", "uid", rec.OwnerUID, "hosts", keys(hosts))
	}
}

// adopt stamps an owner UID on a seeded record (first owner-bearing call).
func (s *state) adopt(rec *vmRecord, uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.OwnerUID != "" {
		return
	}
	rec.OwnerUID = uid
	s.markRunningLocked(rec)
}

// removeRecord drops rec from the indexes.
func (s *state) removeRecord(rec *vmRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ownerKey(rec.Namespace, rec.Name)
	list := s.byOwner[k]
	for i, r := range list {
		if r == rec {
			s.byOwner[k] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.byOwner[k]) == 0 {
		delete(s.byOwner, k)
	}
	if hosts := s.running[rec.OwnerUID]; hosts != nil {
		delete(hosts, rec.Host)
		if len(hosts) == 0 {
			delete(s.running, rec.OwnerUID)
		}
	}
	s.m.vms.Dec()
}

// owned returns the records of owner ns/name on any host.
func (s *state) owned(ns, name string) []vmRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]vmRecord, 0, len(s.byOwner[ownerKey(ns, name)]))
	for _, r := range s.byOwner[ownerKey(ns, name)] {
		out = append(out, *r)
	}
	return out
}

// snapshot copies every record, sorted by host then id.
func (s *state) snapshot() []vmRecord {
	s.mu.RLock()
	agents := make([]*hostAgent, 0, len(s.hosts))
	for _, h := range s.hosts {
		agents = append(agents, h)
	}
	s.mu.RUnlock()
	var out []vmRecord
	for _, h := range agents {
		h.mu.Lock()
		for _, r := range h.vms {
			out = append(out, *r)
		}
		h.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// hostIDs returns the served host ids, sorted.
func (s *state) hostIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.hosts))
	for id := range s.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// keys returns m's keys, sorted.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
