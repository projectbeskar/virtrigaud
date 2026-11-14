//go:build virtrigaud_dev

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

package cloudhypervisor_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/providers/cloudhypervisor"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/agenttarget"
)

func TestMain(m *testing.M) {
	if agenttarget.RunChildIfRequested() {
		return
	}
	os.Exit(m.Run())
}

// The deviations of the clustered Provider for per-host agents, each pinned
// to its failure output so that a different failure, or a pass, fails the
// run.
const (
	// unsignedRefused: the case drives unsigned mutating calls, which this
	// Provider refuses before it routes them.
	unsignedPointer = "the Provider reports requires_operation_identity and refuses every mutating call without an operation grant " +
		"(IDENTITY_REQUIRED) before it routes it; the case drives unsigned mutating calls (identity.* cover the signed path)"
	unsignedText = "this Provider performs a mutating call only with an operation grant"
	// listVMs: a per-host agent serves no ListVMs.
	listPointer = "a per-host agent does not serve ListVMs, so the Provider answers it Unimplemented"
	listText    = "method ListVMs not implemented"
	// ownerUID: a per-host agent keys a VM by its owner's UID.
	ownerPointer = "a per-host agent keys a VM by its owner's UID: the target cannot seed a VM without an owner UID"
	ownerText    = "seeds only VMs with an owner UID"
)

// chExpectedFailures are the cases the clustered Provider for per-host agents
// is known to fail.
var chExpectedFailures = map[string]clustered.ExpectedFailure{
	"route.missing-host":                        {Pointer: unsignedPointer, Contains: unsignedText},
	"route.unknown-host":                        {Pointer: unsignedPointer, Contains: unsignedText},
	"route.invalid-host-id":                     {Pointer: unsignedPointer, Contains: unsignedText},
	"owner.refusal-discloses-nothing":           {Pointer: unsignedPointer, Contains: unsignedText},
	"hostdown.per-vm":                           {Pointer: unsignedPointer, Contains: unsignedText},
	"hostdown.removed-host":                     {Pointer: unsignedPointer, Contains: unsignedText},
	"hostdown.isolated":                         {Pointer: unsignedPointer, Contains: unsignedText},
	"tasks.async-routing":                       {Pointer: unsignedPointer, Contains: unsignedText},
	"tasks.synchronous-effect":                  {Pointer: unsignedPointer, Contains: unsignedText},
	"errinfo.host-unavailable-wire-and-mapping": {Pointer: unsignedPointer, Contains: unsignedText},
	"idempotency.delete":                        {Pointer: unsignedPointer, Contains: unsignedText},
	"idempotency.host-unavailable":              {Pointer: unsignedPointer, Contains: unsignedText},
	"restart.no-resurrection":                   {Pointer: unsignedPointer, Contains: unsignedText},
	"restart.task-ref":                          {Pointer: unsignedPointer, Contains: unsignedText},
	"create.requires-routable-host":             {Pointer: unsignedPointer, Contains: unsignedText},
	"create.routed-to-target-host":              {Pointer: unsignedPointer, Contains: unsignedText},

	"hostdown.blackholed-list":          {Pointer: listPointer, Contains: listText},
	"list.unreachable-host":             {Pointer: listPointer, Contains: listText},
	"list.all-unreachable":              {Pointer: listPointer, Contains: listText},
	"list.recovery":                     {Pointer: listPointer, Contains: listText},
	"list.not-counted-against-provider": {Pointer: listPointer, Contains: listText},
	"idempotency.reads":                 {Pointer: listPointer, Contains: listText},
	"restart.state-lives-on-hosts":      {Pointer: listPointer, Contains: listText},
	"restart.host-down-across-restart":  {Pointer: listPointer, Contains: listText},

	"owner.describe-foreign":      {Pointer: ownerPointer, Contains: ownerText},
	"owner.mutate-foreign":        {Pointer: ownerPointer, Contains: ownerText},
	"owner.request-without-owner": {Pointer: ownerPointer, Contains: ownerText},
	"list.all-hosts":              {Pointer: ownerPointer, Contains: ownerText},
	"route.correct-host": {
		Pointer:  "a per-host agent keys a VM by its owner's UID: another VM of the same owner on the host answers for the name",
		Contains: "OwnerA's VM is on host-a, not on host-b",
	},
	"tasks.forged-inner-ref": {
		Pointer:  "the agent answers a task reference it never issued with InvalidArgument, not NotFound; the Provider relays the agent's answer unchanged",
		Contains: "not a task reference this agent issued",
	},
	"identity.outcome-retired": {
		Pointer:  "a per-host agent records RETIRED only when a re-enrolment freezes an operation that never started; the agent process cannot re-enrol yet",
		Contains: "cannot script this outcome: TASK_OUTCOME_RETIRED",
	},
}

// TestClusteredConformance runs the whole clustered suite against the
// Provider in front of real per-host agent processes with the fake executor.
func TestClusteredConformance(t *testing.T) {
	clustered.Run(t, func(rt clustered.T) clustered.Target {
		tg, err := agenttarget.New(rt, agenttarget.Config{})
		if err != nil {
			rt.Errorf("start the agents: %v", err)
			rt.FailNow()
		}
		return tg
	}, clustered.WithExpectedFailures(chExpectedFailures))
}

// TestRealAgentVersionSkew: with real agent processes, an agent one API
// version behind the Provider is routed, and one two versions behind is
// refused AgentIncompatible without being routed to. The Provider stands for
// a later release through its version override.
func TestRealAgentVersionSkew(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider uint32
		routed   bool
	}{
		{"agent one version behind", answer.AgentAPIVersion + 1, true},
		{"agent two versions behind", answer.AgentAPIVersion + 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg, err := agenttarget.New(t, agenttarget.Config{Hosts: []string{"host-a", "host-b"}, ProviderAPIVersion: tc.provider})
			require.NoError(t, err)
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			srv := tg.NewGRPCServer()
			go func() { _ = srv.Serve(l) }()
			t.Cleanup(srv.Stop)
			cc, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = cc.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err = providerv1.NewProviderClient(cc).Describe(ctx, &providerv1.DescribeRequest{Id: "vm", TargetHostId: "host-a",
				Owner: clustered.OwnerA.Proto()})
			if tc.routed {
				require.NoError(t, err)
				return
			}
			require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
			var cause string
			for _, d := range status.Convert(err).Details() {
				if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == contracts.HostUnavailableReason {
					cause = info.GetMetadata()[cloudhypervisor.MetadataCause]
				}
			}
			assert.Equal(t, cloudhypervisor.CauseAgentIncompatible, cause)
			effects, err := tg.Host("host-a").Effects()
			require.NoError(t, err)
			assert.Empty(t, effects, "nothing is performed on an incompatible agent")
		})
	}
}
