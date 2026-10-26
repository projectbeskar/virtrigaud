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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// fakeAdminPort is the fake provider's admin port (/executions, /config).
const fakeAdminPort = "8090"

// execution mirrors the fake provider's vmRecord JSON.
type execution struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	OwnerUID  string `json:"ownerUID"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	CPU       int32  `json:"cpu"`
	MemoryMiB int64  `json:"memoryMiB"`
	Seeded    bool   `json:"seeded"`
}

// auditReport is the auditor's verdict. Every Violations count must be 0.
type auditReport struct {
	Time            time.Time      `json:"time"`
	VMs             int            `json:"vms"`
	Executions      int            `json:"executions"`
	Bound           int            `json:"bound"`
	Pending         int            `json:"pendingWithoutID"`
	PendingExecuted int            `json:"pendingWithExecution"`
	Violations      map[string]int `json:"violations"`
	Informational   map[string]int `json:"informational"`
	Examples        []string       `json:"examples"`
}

// violation kinds.
const (
	vCapacityAPI       = "api_committed_over_capacity"
	vCapacityExec      = "executed_over_capacity"
	vDuplicateExec     = "owner_executing_on_two_hosts"
	vBoundNotExecuting = "bound_vm_without_execution"
	vExecWrongHost     = "execution_on_host_not_in_placement"
	vOrphanExecution   = "execution_without_vm"
	// seededOverCapacity counts hosts the driver's seed alone put over
	// capacity (no manager placement on them): a fixture fact, not a violation.
	seededOverCapacity = "seeded_hosts_over_capacity"
	maxExamples        = 20
)

// providerPod returns the running pod of a Provider's deployment.
func providerPod(ctx context.Context, e *env, provider string) (string, error) {
	var pods corev1.PodList
	if err := e.c.List(ctx, &pods, client.InNamespace(infraNamespace), client.MatchingLabels{"app.kubernetes.io/instance": provider}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no running pod for Provider %s", provider)
}

// fakeAdmin GETs or POSTs path on a Provider's fake admin port through the
// API server's pod proxy.
func fakeAdmin(ctx context.Context, e *env, provider, path string, body []byte) ([]byte, error) {
	pod, err := providerPod(ctx, e, provider)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(e.cfg)
	if err != nil {
		return nil, err
	}
	req := cs.CoreV1().RESTClient().Get()
	if body != nil {
		req = cs.CoreV1().RESTClient().Post().Body(bytes.NewReader(body))
	}
	return req.Namespace(infraNamespace).Resource("pods").Name(pod + ":" + fakeAdminPort).
		SubResource("proxy").Suffix(path).DoRaw(ctx)
}

// footprint is the size the accounting counts a VM at (recorded, else
// pending, else its class).
func footprint(vm *v1beta1.VirtualMachine) (int64, int64) {
	if cur := vm.Status.CurrentResources; cur != nil && cur.CPU != nil && cur.MemoryMiB != nil {
		return int64(*cur.CPU), *cur.MemoryMiB
	}
	if pl := vm.Status.Placement; pl != nil && pl.PendingResources != nil {
		return int64(pl.PendingResources.CPU), pl.PendingResources.MemoryMiB
	}
	c, _ := workload.ClassByName(vm.Spec.ClassRef.Name)
	return int64(c.CPU), c.MemoryMiB
}

// auditCmd compares desired and placement state with execution state.
func auditCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	out := fs.String("out", "", "write the report here as JSON")
	return func(ctx context.Context, e *env) error {
		l, err := loadLayout(ctx, e)
		if err != nil {
			return err
		}
		r := auditReport{Time: time.Now(), Violations: map[string]int{}, Informational: map[string]int{}}
		note := func(kind, example string) {
			r.Violations[kind]++
			if len(r.Examples) < maxExamples {
				r.Examples = append(r.Examples, kind+": "+example)
			}
		}
		var vms v1beta1.VirtualMachineList
		if err := e.c.List(ctx, &vms); err != nil {
			return err
		}
		byUID := map[string]*v1beta1.VirtualMachine{}
		byKey := map[string]*v1beta1.VirtualMachine{}
		for i := range vms.Items {
			vm := &vms.Items[i]
			byUID[string(vm.UID)] = vm
			byKey[vm.Namespace+"/"+vm.Name] = vm
		}
		r.VMs = len(vms.Items)
		for _, pl := range l.Providers {
			raw, err := fakeAdmin(ctx, e, pl.Name, "executions", nil)
			if err != nil {
				return fmt.Errorf("executions of %s: %w", pl.Name, err)
			}
			var execs []execution
			if err := json.Unmarshal(raw, &execs); err != nil {
				return fmt.Errorf("parse executions of %s: %w", pl.Name, err)
			}
			r.Executions += len(execs)
			auditProvider(l, pl, vms.Items, byUID, byKey, execs, &r, note)
		}
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		if *out != "" {
			if err := os.WriteFile(*out, data, 0o600); err != nil {
				return err
			}
		}
		return nil
	}
}

// auditProvider checks one Provider's VMs against its fake executions.
func auditProvider(l *layout, pl providerLayout, vms []v1beta1.VirtualMachine, byUID, byKey map[string]*v1beta1.VirtualMachine,
	execs []execution, r *auditReport, note func(string, string)) {
	type usage struct{ cpu, mem int64 }
	apiUse := map[string]usage{}
	managed := map[string]bool{} // hosts holding a VM the manager placed
	execUse := map[string]usage{}
	execByHostID := map[string]execution{}
	ownerHosts := map[string]map[string]bool{}

	for _, x := range execs {
		u := execUse[x.Host]
		u.cpu += int64(x.CPU)
		u.mem += x.MemoryMiB
		execUse[x.Host] = u
		execByHostID[x.Host+"|"+x.ID] = x
		owner := x.OwnerUID
		if owner == "" {
			owner = "seed:" + x.Namespace + "/" + x.Name
		}
		if ownerHosts[owner] == nil {
			ownerHosts[owner] = map[string]bool{}
		}
		ownerHosts[owner][x.Host] = true

		// The VM this execution belongs to, by UID (or by name for a
		// seeded record not adopted yet).
		vm := byUID[x.OwnerUID]
		if x.OwnerUID == "" {
			vm = byKey[x.Namespace+"/"+x.Name]
		}
		if vm == nil {
			note(vOrphanExecution, fmt.Sprintf("%s on %s (owner %s)", x.ID, x.Host, x.OwnerUID))
			continue
		}
		pl := vm.Status.Placement
		if pl == nil || (pl.Host != x.Host && pl.PendingHost != x.Host) {
			note(vExecWrongHost, fmt.Sprintf("%s/%s executes on %s; placement %+v", vm.Namespace, vm.Name, x.Host, pl))
		}
	}
	for owner, hosts := range ownerHosts {
		if len(hosts) > 1 {
			note(vDuplicateExec, fmt.Sprintf("%s on %v", owner, hostList(hosts)))
		}
	}
	for i := range vms {
		vm := &vms[i]
		if vm.Spec.ProviderRef.Name != pl.Name {
			continue
		}
		p := vm.Status.Placement
		if p == nil {
			continue
		}
		cpu, mem := footprint(vm)
		hosts := map[string]bool{}
		if p.Host != "" {
			hosts[p.Host] = true
		}
		if p.PendingHost != "" {
			hosts[p.PendingHost] = true
		}
		for h := range hosts {
			if vm.Labels[labelRun] != seedRun {
				managed[h] = true
			}
			u := apiUse[h]
			u.cpu += cpu
			u.mem += mem
			apiUse[h] = u
		}
		switch {
		case vm.Status.ID != "":
			r.Bound++
			x, ok := execByHostID[p.Host+"|"+vm.Status.ID]
			if !ok || (x.OwnerUID != "" && x.OwnerUID != string(vm.UID)) {
				if vm.DeletionTimestamp == nil {
					note(vBoundNotExecuting, fmt.Sprintf("%s/%s id %s host %s", vm.Namespace, vm.Name, vm.Status.ID, p.Host))
				}
			}
		case p.PendingHost != "":
			r.Pending++
			if _, ok := execByHostID[p.PendingHost+"|"+workload.DomainID(vm.Namespace, vm.Name)]; ok {
				r.PendingExecuted++
			}
		}
	}
	over := func(u usage) bool { return u.cpu > int64(l.HostCPU) || u.mem > l.HostMemMiB }
	for h, u := range apiUse {
		switch {
		case over(u) && managed[h]:
			note(vCapacityAPI, fmt.Sprintf("%s: %d vCPU / %d MiB", h, u.cpu, u.mem))
		case over(u):
			r.Informational[seededOverCapacity]++
		}
	}
	for h, u := range execUse {
		if over(u) && managed[h] {
			note(vCapacityExec, fmt.Sprintf("%s: %d vCPU / %d MiB", h, u.cpu, u.mem))
		}
	}
}

// hostList returns the hosts in m, sorted.
func hostList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
