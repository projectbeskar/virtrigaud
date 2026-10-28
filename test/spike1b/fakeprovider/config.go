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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables the fake reads at start. Everything except the seed
// can also be changed at run time through POST /config on the admin port,
// so a fault or latency change does not restart the pod (a restart would
// lose every VM created since the seed).
const (
	envSeed             = "FAKE_SEED"
	envHostCPU          = "FAKE_HOST_CPU"
	envHostMemMiB       = "FAKE_HOST_MEM_MIB"
	envLatency          = "FAKE_LATENCY"
	envHostConcurrency  = "FAKE_HOST_CONCURRENCY"
	envUnreachableHosts = "FAKE_UNREACHABLE_HOSTS"
	envCreateFailBefore = "FAKE_CREATE_FAIL_BEFORE"
	envCreateLostReply  = "FAKE_CREATE_LOST_REPLY"
	envReportIPs        = "FAKE_REPORT_IPS"
	envAdminAddr        = "FAKE_ADMIN_ADDR"
)

// RPC method names used as latency keys and metric labels.
const (
	methodValidate    = "validate"
	methodCaps        = "capabilities"
	methodHostInfo    = "hostinfo"
	methodListHosts   = "listhosts"
	methodCreate      = "create"
	methodDescribe    = "describe"
	methodDelete      = "delete"
	methodPower       = "power"
	methodListVMs     = "listvms"
	methodTaskStatus  = "taskstatus"
	defaultAdminAddr  = ":8090"
	defaultHostCPU    = 256
	defaultHostMemMiB = 1 << 20
)

// latency is a mean and a uniform jitter (mean ± jitter).
type latency struct {
	Mean   time.Duration `json:"mean"`
	Jitter time.Duration `json:"jitter"`
}

// sample draws one latency.
func (l latency) sample() time.Duration {
	if l.Jitter <= 0 {
		return l.Mean
	}
	d := l.Mean - l.Jitter + time.Duration(rand.Int64N(int64(2*l.Jitter)+1)) // #nosec G404 -- simulated latency
	return max(d, 0)
}

// config is the fake's behaviour. It is replaced as a whole (atomic pointer).
type config struct {
	// Latency is the simulated host-side latency per method.
	Latency map[string]latency `json:"latency"`
	// HostConcurrency is how many operations one host's agent executes at
	// once (create, delete, power); further operations queue.
	HostConcurrency int `json:"hostConcurrency"`
	// HostCPU and HostMemMiB are what GetHostInfo reports for every host.
	HostCPU int32 `json:"hostCPU"`
	// HostMemMiB is the host memory reported.
	HostMemMiB int64 `json:"hostMemMiB"`
	// UnreachableHosts answer every call with HOST_UNAVAILABLE.
	UnreachableHosts []string `json:"unreachableHosts"`
	// CreateFailBefore is the probability that a Create fails with
	// HOST_UNAVAILABLE before doing anything.
	CreateFailBefore float64 `json:"createFailBefore"`
	// CreateLostReply is the probability that a Create executes and then
	// answers OUTCOME_UNKNOWN_RETRY, as if its reply were lost.
	CreateLostReply float64 `json:"createLostReply"`
	// ReportIPs makes Describe report an address (a guest agent).
	ReportIPs bool `json:"reportIPs"`
}

// defaultConfig is the configuration without any environment.
func defaultConfig() *config {
	return &config{
		Latency: map[string]latency{
			methodValidate:   {Mean: time.Millisecond},
			methodCaps:       {Mean: time.Millisecond},
			methodHostInfo:   {Mean: 20 * time.Millisecond, Jitter: 10 * time.Millisecond},
			methodListHosts:  {Mean: 20 * time.Millisecond},
			methodCreate:     {Mean: 200 * time.Millisecond, Jitter: 100 * time.Millisecond},
			methodDescribe:   {Mean: 5 * time.Millisecond, Jitter: 3 * time.Millisecond},
			methodDelete:     {Mean: 100 * time.Millisecond, Jitter: 50 * time.Millisecond},
			methodPower:      {Mean: 50 * time.Millisecond, Jitter: 20 * time.Millisecond},
			methodListVMs:    {Mean: 20 * time.Millisecond},
			methodTaskStatus: {Mean: time.Millisecond},
		},
		HostConcurrency: 8,
		HostCPU:         defaultHostCPU,
		HostMemMiB:      defaultHostMemMiB,
		ReportIPs:       true,
	}
}

// configFromEnv overlays the environment on defaultConfig. FAKE_LATENCY is
// "method=mean[/jitter],..." in Go durations, e.g. "create=500ms/200ms".
func configFromEnv() (*config, error) {
	c := defaultConfig()
	if v := os.Getenv(envLatency); v != "" {
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok {
				return nil, fmt.Errorf("%s: %q is not method=duration", envLatency, kv)
			}
			l, err := parseLatency(val)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", envLatency, k, err)
			}
			c.Latency[k] = l
		}
	}
	intEnv := func(name string, dst *int) error {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*dst = n
		}
		return nil
	}
	if err := intEnv(envHostConcurrency, &c.HostConcurrency); err != nil {
		return nil, err
	}
	cpu := int(c.HostCPU)
	if err := intEnv(envHostCPU, &cpu); err != nil {
		return nil, err
	}
	c.HostCPU = int32(min(cpu, 1<<30)) // #nosec G115 -- bounded above
	mem := int(c.HostMemMiB)
	if err := intEnv(envHostMemMiB, &mem); err != nil {
		return nil, err
	}
	c.HostMemMiB = int64(mem)
	if v := os.Getenv(envUnreachableHosts); v != "" {
		c.UnreachableHosts = strings.Split(v, ",")
	}
	floatEnv := func(name string, dst *float64) error {
		if v := os.Getenv(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*dst = f
		}
		return nil
	}
	if err := floatEnv(envCreateFailBefore, &c.CreateFailBefore); err != nil {
		return nil, err
	}
	if err := floatEnv(envCreateLostReply, &c.CreateLostReply); err != nil {
		return nil, err
	}
	if v := os.Getenv(envReportIPs); v != "" {
		c.ReportIPs = v == "true"
	}
	return c, nil
}

// parseLatency reads "mean[/jitter]".
func parseLatency(v string) (latency, error) {
	meanS, jitterS, _ := strings.Cut(v, "/")
	mean, err := time.ParseDuration(meanS)
	if err != nil {
		return latency{}, err
	}
	l := latency{Mean: mean}
	if jitterS != "" {
		if l.Jitter, err = time.ParseDuration(jitterS); err != nil {
			return latency{}, err
		}
	}
	return l, nil
}

// MarshalJSON renders durations as strings for the admin endpoint.
func (l latency) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Mean   string `json:"mean"`
		Jitter string `json:"jitter"`
	}{l.Mean.String(), l.Jitter.String()})
}

// UnmarshalJSON reads durations written as strings.
func (l *latency) UnmarshalJSON(b []byte) error {
	var raw struct {
		Mean   string `json:"mean"`
		Jitter string `json:"jitter"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	parsed, err := parseLatency(raw.Mean + "/" + raw.Jitter)
	if raw.Jitter == "" {
		parsed, err = parseLatency(raw.Mean)
	}
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}
