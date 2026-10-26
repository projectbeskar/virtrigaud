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
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// hostsCmd samples Host heartbeat ages every --interval for --duration and
// writes one CSV row per sample: count, Ready count and age percentiles.
func hostsCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	interval := fs.Duration("interval", 15*time.Second, "sample interval")
	duration := fs.Duration("duration", 5*time.Minute, "how long to sample")
	out := fs.String("out", "hosts.csv", "CSV output")
	return func(ctx context.Context, e *env) error {
		rows := [][]string{{"t_s", "hosts", "ready", "no_heartbeat", "age_p50_s", "age_p90_s", "age_max_s"}}
		start := time.Now()
		for time.Since(start) < *duration {
			var hosts v1beta1.HostList
			if err := e.c.List(ctx, &hosts, client.InNamespace(infraNamespace)); err != nil {
				return err
			}
			now := time.Now()
			var ages []float64
			ready, none := 0, 0
			for _, h := range hosts.Items {
				if h.Status.Health == v1beta1.HostHealthReady {
					ready++
				}
				if h.Status.LastHeartbeatTime == nil {
					none++
					continue
				}
				ages = append(ages, now.Sub(h.Status.LastHeartbeatTime.Time).Seconds())
			}
			slices.Sort(ages)
			pct := func(q float64) string {
				if len(ages) == 0 {
					return ""
				}
				return fmt.Sprintf("%.1f", ages[min(len(ages)-1, int(q*float64(len(ages))))])
			}
			row := []string{fmt.Sprintf("%.0f", now.Sub(start).Seconds()), strconv.Itoa(len(hosts.Items)), strconv.Itoa(ready),
				strconv.Itoa(none), pct(0.5), pct(0.9), pct(1)}
			rows = append(rows, row)
			fmt.Println(strings.Join(row, ","))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(*interval):
			}
		}
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return csv.NewWriter(f).WriteAll(rows)
	}
}

// statusCmd counts VMs by Ready status and Ready/Placed reason.
func statusCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	asJSON := fs.Bool("json", false, "print JSON")
	return func(ctx context.Context, e *env) error {
		var vms v1beta1.VirtualMachineList
		if err := e.c.List(ctx, &vms); err != nil {
			return err
		}
		counts := map[string]int{}
		for _, vm := range vms.Items {
			k := "Ready=missing"
			if c := meta.FindStatusCondition(vm.Status.Conditions, readyCond); c != nil {
				k = fmt.Sprintf("Ready=%s/%s", c.Status, c.Reason)
			}
			counts[k]++
			if c := meta.FindStatusCondition(vm.Status.Conditions, placedCond); c != nil {
				counts["Placed="+string(c.Status)+"/"+c.Reason]++
			}
			if !vm.DeletionTimestamp.IsZero() {
				counts["deleting"]++
			}
		}
		counts["total"] = len(vms.Items)
		if *asJSON {
			data, _ := json.Marshal(counts)
			fmt.Println(string(data))
			return nil
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Printf("%-50s %d\n", k, counts[k])
		}
		return nil
	}
}

// cleanupCmd deletes every VM labelled with --run (and waits for them to go).
func cleanupCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	run := fs.String("run", "", "run label value; required")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the VMs to go")
	return func(ctx context.Context, e *env) error {
		if *run == "" {
			return fmt.Errorf("--run is required")
		}
		sel := client.MatchingLabels{labelRun: *run}
		var vms v1beta1.VirtualMachineList
		if err := e.c.List(ctx, &vms, sel); err != nil {
			return err
		}
		_ = parallel(ctx, len(vms.Items), 50, func(i int) error {
			vm := &vms.Items[i]
			_ = e.c.Delete(ctx, &v1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name}})
			return nil
		})
		deadline := time.Now().Add(*wait)
		for time.Now().Before(deadline) {
			if err := e.c.List(ctx, &vms, sel); err != nil {
				return err
			}
			if len(vms.Items) == 0 {
				fmt.Printf("cleanup %s: done\n", *run)
				return nil
			}
			time.Sleep(5 * time.Second)
		}
		return fmt.Errorf("cleanup %s: %d VMs still present", *run, len(vms.Items))
	}
}

// fakeConfigCmd reads or changes a fake provider's configuration.
func fakeConfigCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	provider := fs.String("provider", "fake-p0", "Provider whose fake to configure")
	body := fs.String("json", "", "JSON to POST (merged into the current config); empty only reads")
	return func(ctx context.Context, e *env) error {
		var payload []byte
		if *body != "" {
			payload = []byte(*body)
		}
		out, err := fakeAdmin(ctx, e, *provider, "config", payload)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
}
