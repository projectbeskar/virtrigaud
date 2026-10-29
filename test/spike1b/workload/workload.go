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

// Package workload is the deterministic workload model Spike 1B's driver
// and fake provider share (docs/performance/spike-1b-control-plane-baseline.md).
//
// A Spec names a set of pre-populated ("seeded") VirtualMachines: the
// driver writes them to the API server with a steady-state status, and the
// fake provider builds the matching execution state from the same Spec, so
// the first reconcile finds every VM already running on its host. Every
// value is a pure function of the Spec and the VM's index.
//
// Test-only: no production binary imports this package.
package workload

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// Class is one VMClass shape.
type Class struct {
	// Name is the VMClass name (created in every tenant namespace).
	Name string
	// CPU is its vCPU count.
	CPU int32
	// MemoryMiB is its memory in MiB.
	MemoryMiB int64
}

// Classes are the VMClass shapes, by index.
var Classes = []Class{
	{Name: "small", CPU: 1, MemoryMiB: 1024},
	{Name: "medium", CPU: 2, MemoryMiB: 4096},
	{Name: "large", CPU: 4, MemoryMiB: 16384},
	{Name: "xlarge", CPU: 8, MemoryMiB: 32768},
}

// classByMod maps i mod 10 to a class index: 40% small, 30% medium, 20%
// large, 10% xlarge.
var classByMod = [10]int{0, 0, 0, 0, 1, 1, 1, 2, 2, 3}

// ClassFor returns the class of seeded VM i.
func ClassFor(i int) Class { return Classes[classByMod[i%len(classByMod)]] }

// ClassByName returns the class called name.
func ClassByName(name string) (Class, bool) {
	for _, c := range Classes {
		if c.Name == name {
			return c, true
		}
	}
	return Class{}, false
}

// Spec describes a seeded population.
type Spec struct {
	// VMPrefix prefixes every seeded VM name.
	VMPrefix string
	// Count is how many VMs are seeded.
	Count int
	// Namespaces is how many tenant namespaces they are spread over.
	Namespaces int
	// HostPrefix prefixes every Host name.
	HostPrefix string
	// Hosts is how many Hosts the VMs are spread over (round-robin).
	Hosts int
}

// String renders the Spec in the form ParseSpec reads.
func (s Spec) String() string {
	return fmt.Sprintf("vmprefix=%s,count=%d,ns=%d,hostprefix=%s,hosts=%d",
		s.VMPrefix, s.Count, s.Namespaces, s.HostPrefix, s.Hosts)
}

// ParseSpec reads a Spec written by String. An empty string is the empty
// Spec (nothing seeded).
func ParseSpec(v string) (Spec, error) {
	var s Spec
	if strings.TrimSpace(v) == "" {
		return s, nil
	}
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return s, fmt.Errorf("workload spec: %q is not key=value", kv)
		}
		var err error
		switch k {
		case "vmprefix":
			s.VMPrefix = val
		case "hostprefix":
			s.HostPrefix = val
		case "count":
			s.Count, err = strconv.Atoi(val)
		case "ns":
			s.Namespaces, err = strconv.Atoi(val)
		case "hosts":
			s.Hosts, err = strconv.Atoi(val)
		default:
			return s, fmt.Errorf("workload spec: unknown key %q", k)
		}
		if err != nil {
			return s, fmt.Errorf("workload spec: %s: %w", k, err)
		}
	}
	if s.Count > 0 && (s.Namespaces < 1 || s.Hosts < 1) {
		return s, fmt.Errorf("workload spec: count %d needs ns and hosts > 0", s.Count)
	}
	return s, nil
}

// VM is one seeded VM.
type VM struct {
	// Name and Namespace identify the VirtualMachine.
	Name string
	// Namespace is its tenant namespace.
	Namespace string
	// Host is the Host it runs on.
	Host string
	// ID is its provider id (status.id).
	ID string
	// IP is the address the fake provider reports for it.
	IP string
	// Class is its VMClass shape.
	Class Class
}

// VM returns seeded VM i.
func (s Spec) VM(i int) VM {
	name := fmt.Sprintf("%s%06d", s.VMPrefix, i)
	ns := Namespace(i % s.Namespaces)
	return VM{
		Name:      name,
		Namespace: ns,
		Host:      HostName(s.HostPrefix, i%s.Hosts),
		ID:        DomainID(ns, name),
		IP:        IPFor(ns, name),
		// Decorrelated from the host (i mod Hosts): with Hosts a multiple of
		// 10, ClassFor(i) alone would give every VM of a host the same class.
		Class: ClassFor(i + i/s.Hosts),
	}
}

// Namespace returns tenant namespace i.
func Namespace(i int) string { return fmt.Sprintf("tenant-%02d", i) }

// HostName returns host i of a pool whose Host names start with prefix.
func HostName(prefix string, i int) string { return fmt.Sprintf("%s%04d", prefix, i) }

// DomainID is the provider id of the VM ns/name: the clustered libvirt
// provider's namespaced domain name, "<namespace>.<name>".
func DomainID(ns, name string) string { return ns + "." + name }

// IPFor returns a stable 10.x.y.z address for ns/name.
func IPFor(ns, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ns + "/" + name))
	v := h.Sum32()
	return fmt.Sprintf("10.%d.%d.%d", (v>>16)&0xff, (v>>8)&0xff, 1+v%254)
}
