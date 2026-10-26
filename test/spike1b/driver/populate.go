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
	"flag"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// seedRun labels seeded VMs.
const seedRun = "seed"

// populateCmd writes the seeded VMs with the status a bound, running VM has.
// Run it with the manager scaled to 0: a VM the manager sees before its
// status lands would be scheduled and created a second time.
func populateCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	workers := fs.Int("workers", 64, "concurrent writers")
	return func(ctx context.Context, e *env) error {
		l, err := loadLayout(ctx, e)
		if err != nil {
			return err
		}
		start := time.Now()
		for _, pl := range l.Providers {
			var prov v1beta1.Provider
			if err := e.c.Get(ctx, client.ObjectKey{Namespace: infraNamespace, Name: pl.Name}, &prov); err != nil {
				return fmt.Errorf("get Provider %s: %w", pl.Name, err)
			}
			spec := pl.Seed
			fmt.Printf("populating %d VMs for %s\n", spec.Count, pl.Name)
			err := parallel(ctx, spec.Count, *workers, func(i int) error {
				return populateOne(ctx, e, &prov, pl, spec.VM(i))
			})
			if err != nil {
				return err
			}
		}
		fmt.Printf("populate done in %s\n", time.Since(start).Round(time.Second))
		return nil
	}
}

// populateOne creates one seeded VM, records its bound status and stamps
// the placement-uid restore marker the manager would have written.
func populateOne(ctx context.Context, e *env, prov *v1beta1.Provider, pl providerLayout, v workload.VM) error {
	vm := &v1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: v.Name, Namespace: v.Namespace, Labels: map[string]string{labelRun: seedRun}},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.ObjectRef{Name: pl.Name, Namespace: infraNamespace},
			ClassRef:    v1beta1.ObjectRef{Name: v.Class.Name},
			ImageRef:    &v1beta1.ObjectRef{Name: imageName},
			PowerState:  v1beta1.PowerStateOn,
		},
	}
	if err := e.c.Create(ctx, vm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s/%s: %w", v.Namespace, v.Name, err)
		}
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(vm), vm); err != nil {
			return err
		}
	}
	if vm.Status.ID == "" {
		cpu, mem, zero := v.Class.CPU, v.Class.MemoryMiB, int64(0)
		vm.Status.ID = v.ID
		vm.Status.BoundProvider = &v1beta1.BoundProviderRef{Namespace: infraNamespace, Name: pl.Name, UID: string(prov.UID)}
		vm.Status.Placement = &v1beta1.PlacementStatus{Host: v.Host, Pool: pl.Name + "-" + poolName, MemoryCeilingMiB: &zero}
		vm.Status.CurrentResources = &v1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
		vm.Status.IPs = []string{v.IP}
		if err := e.c.Status().Update(ctx, vm); err != nil {
			return fmt.Errorf("status %s/%s: %w", v.Namespace, v.Name, err)
		}
	}
	if vm.Annotations[v1beta1.VirtualMachinePlacementUIDAnnotation] != string(vm.UID) {
		patch := client.MergeFrom(vm.DeepCopy())
		if vm.Annotations == nil {
			vm.Annotations = map[string]string{}
		}
		vm.Annotations[v1beta1.VirtualMachinePlacementUIDAnnotation] = string(vm.UID)
		if err := e.c.Patch(ctx, vm, patch); err != nil {
			return fmt.Errorf("annotate %s/%s: %w", v.Namespace, v.Name, err)
		}
	}
	return nil
}
