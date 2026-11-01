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

package clustered

import (
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	// HostTaskPrefix starts every host-encoded task reference
	// (host-task/v1/<host id>/<host-local reference>).
	HostTaskPrefix = "host-task/v1/"

	srcTaskRefs = "ADR-0007 Addendum A, A1 (\"a clustered provider's async task refs must include the host id\") and its slice 3 note; docs/clustered-provider-inventory.md (host-encoded task references)"
)

func taskCases() []Case {
	return []Case{
		{
			ID:     "tasks.malformed-ref",
			Rule:   "TaskStatus refuses a reference that is not host-encoded with InvalidArgument, and a host-encoded one with a malformed host id with InvalidArgument or NotFound: never a task result (done or failed), never a dial",
			Source: srcTaskRefs + " (\"one that is not host-encoded or is malformed is InvalidArgument\"; a host outside the registry is NotFound)",
			Run: func(e *Env) {
				a := e.HostA()
				// Not host-encoded at all: InvalidArgument is required.
				notEncoded := map[string]string{
					"empty":            "",
					"not host-encoded": "task-123",
					"other version":    "host-task/v2/" + a + "/t",
				}
				for name, ref := range notEncoded {
					resp, err := e.TaskStatus(ref)
					require.Error(e.T, err, "%s: a forged reference must not be reported as a task result (got %v)", name, resp)
					requireCode(e, err, codes.InvalidArgument, name)
				}
				// Host-encoded, but with a structure or a host id that cannot be one
				// of this provider's: refused as InvalidArgument or, for a host id
				// that is merely not in the registry, NotFound. Never a result.
				badHost := map[string]string{
					"no task":                  HostTaskPrefix + a,
					"empty task":               HostTaskPrefix + a + "/",
					"empty host":               HostTaskPrefix + "/t",
					"uppercase host":           HostTaskPrefix + strings.ToUpper(a) + "/t",
					"host with a port":         HostTaskPrefix + a + ":22/t",
					"host is a path":           HostTaskPrefix + "../t",
					"host is an endpoint":      HostTaskPrefix + "qemu+ssh:/t",
					"host with leading hyphen": HostTaskPrefix + "-host/t",
					"too long":                 HostTaskPrefix + a + "/" + strings.Repeat("x", 2048),
				}
				for name, ref := range badHost {
					resp, err := e.TaskStatus(ref)
					require.Error(e.T, err, "%s: a forged reference must not be reported as a task result (got %v)", name, resp)
					w := WireOf(err)
					assert.Contains(e.T, []codes.Code{codes.InvalidArgument, codes.NotFound}, w.Code, "%s: %s", name, w)
				}
				// A request with no task at all.
				ctx, cancel := e.Call()
				defer cancel()
				_, err := e.Raw().TaskStatus(ctx, &providerv1.TaskStatusRequest{})
				requireCode(e, err, codes.InvalidArgument, "no task")

				// The manager maps it to a non-retryable InvalidSpec.
				_, err = e.Manager().IsTaskComplete(e.Ctx(), "task-123")
				require.Error(e.T, err)
				assert.True(e.T, contracts.IsInvalidSpec(err), "manager mapping: %v", err)
				requireNeverDialed(e, "", "..", "-host", "qemu+ssh:")
			},
		},
		{
			ID:     "tasks.unknown-host",
			Rule:   "TaskStatus for a well-formed reference naming a host outside the provider's registry is NotFound: never done, never an endpoint, never dialed",
			Source: srcTaskRefs + " (\"names a host that is not in its own host registry is NotFound — the reference is never turned into an endpoint and nothing is dialed\")",
			Run: func(e *Env) {
				for _, host := range []string{UnknownHost, "attacker.example.com"} {
					resp, err := e.TaskStatus(HostTaskPrefix + host + "/job-1")
					require.Error(e.T, err, "%s: must be refused (got %v)", host, resp)
					requireCode(e, err, codes.NotFound, "task of host "+host)
				}
				_, err := e.Manager().IsTaskComplete(e.Ctx(), HostTaskPrefix+UnknownHost+"/job-1")
				assert.True(e.T, contracts.IsNotFound(err), "manager mapping: %v", err)
				requireNeverDialed(e, UnknownHost, "attacker.example.com")
			},
		},
		{
			ID:     "tasks.forged-inner-ref",
			Level:  Recommended,
			Rule:   "a well-formed reference to a reachable host it fronts, but to a task it never issued, is refused (NotFound or InvalidArgument), not reported done",
			Source: srcTaskRefs + " (\"refuses any reference it could not have issued\"); the contract does not say how a provider that issues no tasks can tell",
			Run: func(e *Env) {
				resp, err := e.TaskStatus(HostTaskPrefix + e.HostA() + "/never-issued-0000")
				require.Error(e.T, err, "a task that was never issued must not be reported (got %v)", resp)
				w := WireOf(err)
				assert.Contains(e.T, []codes.Code{codes.NotFound, codes.InvalidArgument}, w.Code, w.String())
			},
		},
		{
			ID:     "tasks.host-down-ref",
			Rule:   "TaskStatus for a well-formed reference naming a host the provider fronts but cannot reach is the retryable, host-scoped Unavailable that names the host: not NotFound, not a task result",
			Source: "ADR-0007 Addendum A, A1 (task references are routed by host) and A2 (an unreachable host is a retryable Unavailable); internal/providers/contracts/errors.go: HostUnavailableReason",
			Run: func(e *Env) {
				e.HostDown(e.HostA())
				ref := HostTaskPrefix + e.HostA() + "/job-1"
				resp, err := e.TaskStatus(ref)
				require.Error(e.T, err, "an unreachable host cannot say the task is done (got %v)", resp)
				requireHostUnavailable(e, err, "", "task of an unreachable host")
				_, err = e.Manager().IsTaskComplete(e.Ctx(), ref)
				assert.True(e.T, contracts.IsHostUnavailable(err), "manager mapping: %v", err)
				assert.False(e.T, contracts.IsNotFound(err))
			},
		},
		{
			ID:       "tasks.async-routing",
			Rule:     "an asynchronous call returns a host-encoded task reference naming the host it ran on; TaskStatus is routed to that host (done only when that host finished it) and a task of an unreachable host is host-scoped Unavailable while other hosts' tasks still answer",
			Source:   srcTaskRefs,
			Requires: []Feature{FeatureAsyncTasks},
			Run: func(e *Env) {
				ctl, ok := e.Target.(TaskController)
				require.True(e.T, ok, "a target offering %s implements TaskController", FeatureAsyncTasks)
				onA := e.Seed(SeedSpec{Host: e.HostA(), Name: "task-a", Owner: OwnerA, Running: false})
				onB := e.Seed(SeedSpec{Host: e.HostB(), Name: "task-b", Owner: OwnerA, Running: false})
				refOf := func(vm VM) string {
					resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
					require.NoError(e.T, err)
					require.NotNil(e.T, resp.GetTask(), "the Target declares asynchronous power operations")
					ref := resp.GetTask().GetId()
					assert.True(e.T, strings.HasPrefix(ref, HostTaskPrefix+vm.Host+"/"),
						"task reference %q must be host-encoded with %s", ref, vm.Host)
					return ref
				}
				refA, refB := refOf(onA), refOf(onB)

				resp, err := e.TaskStatus(refA)
				require.NoError(e.T, err)
				assert.False(e.T, resp.Done, "the host has not finished the task yet")
				require.NoError(e.T, ctl.FinishTasks())
				resp, err = e.TaskStatus(refA)
				require.NoError(e.T, err)
				assert.True(e.T, resp.Done, "the host finished the task")
				assert.Empty(e.T, resp.Error)
				assert.True(e.T, e.Observe(onA).Running, "a task reported done has had its effect")
				done, err := e.Manager().IsTaskComplete(e.Ctx(), refB)
				require.NoError(e.T, err)
				assert.True(e.T, done)

				// A task is routed to the host in its reference.
				e.HostDown(e.HostA())
				_, err = e.TaskStatus(refA)
				requireHostUnavailable(e, err, "", "task of an unreachable host")
				resp, err = e.TaskStatus(refB)
				require.NoError(e.T, err, "the task of a reachable host still answers")
				assert.True(e.T, resp.Done)
			},
		},
		{
			ID:     "tasks.synchronous-effect",
			Rule:   "a routed call that returns no task has completed: its effect is already in place when the response arrives; a task it does return is host-encoded with the request's host",
			Source: "proto/provider/v1/provider.proto: TaskResponse (\"Generic task response for async operations\"); " + srcTaskRefs,
			Run: func(e *Env) {
				vm := e.Seed(SeedSpec{Host: e.HostA(), Name: "sync-vm", Owner: OwnerA, Running: false})
				resp, err := e.Power(vm.Host, vm.ID, OwnerA, providerv1.PowerOp_POWER_OP_ON)
				require.NoError(e.T, err)
				if ref := resp.GetTask().GetId(); ref != "" {
					assert.True(e.T, strings.HasPrefix(ref, HostTaskPrefix+vm.Host+"/"), "task reference %q is host-encoded", ref)
				} else {
					assert.True(e.T, e.Observe(vm).Running, "no task was returned, so the VM must already be on")
				}
				e.Settle(resp)
				assert.True(e.T, e.Observe(vm).Running, "the VM is on once the call, or its task, is done")
				resp, err = e.Delete(vm.Host, vm.ID, OwnerA)
				require.NoError(e.T, err)
				if ref := resp.GetTask().GetId(); ref != "" {
					assert.True(e.T, strings.HasPrefix(ref, HostTaskPrefix+vm.Host+"/"), "task reference %q is host-encoded", ref)
				} else {
					assert.False(e.T, e.Observe(vm).Exists, "no task was returned, so the VM must already be gone")
				}
				e.Settle(resp)
				assert.False(e.T, e.Observe(vm).Exists, "the VM is gone once the call, or its task, is done")
			},
		},
	}
}
