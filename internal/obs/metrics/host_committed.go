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

// Committed capacity of a clustered Provider's hosts (ADR-0007 Addendum A,
// scheduler-accuracy amendment): what the operator's VirtualMachines bound to,
// or pending on, each Host hold — the sum the scheduler subtracts from the
// host's capacity. The labels are the Provider ("namespace/name") and the Host
// name only, so the series count is the number of Hosts. Refreshed by the Host
// controller on every Host sync (about once a minute) and removed with the
// Host.
var (
	hostCommittedCPU = registerer.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_host_committed_cpu",
			Help: "vCPUs committed to a clustered Provider's Host by the VirtualMachines bound to or pending on it (the scheduler's committed capacity), by provider (namespace/name) and host.",
		},
		[]string{"provider", "host"},
	)
	hostCommittedMemoryMiB = registerer.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "virtrigaud_host_committed_memory_mib",
			Help: "Memory in MiB committed to a clustered Provider's Host by the VirtualMachines bound to or pending on it (the scheduler's committed capacity), by provider (namespace/name) and host.",
		},
		[]string{"provider", "host"},
	)
)

// SetHostCommitted publishes the committed vCPUs and MiB of host on provider
// ("namespace/name").
func SetHostCommitted(provider, host string, cpu, memMiB int64) {
	hostCommittedCPU.WithLabelValues(provider, host).Set(float64(cpu))
	hostCommittedMemoryMiB.WithLabelValues(provider, host).Set(float64(memMiB))
}

// DeleteHostCommitted removes host's committed-capacity series, when the Host
// is deleted.
func DeleteHostCommitted(provider, host string) {
	hostCommittedCPU.DeleteLabelValues(provider, host)
	hostCommittedMemoryMiB.DeleteLabelValues(provider, host)
}
