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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the signed Power and Reconfigure of a VM of a Provider that
// requires operation identity, and the serialisation of attempts across
// kinds, against the scripted client (identityStub). The envtest specs run
// the same lifecycle against the instrumented fake over gRPC.

// setVMSpec edits the VM's spec.
func setVMSpec(t *testing.T, r *VirtualMachineReconciler, edit func(*infravirtrigaudiov1beta1.VirtualMachine)) {
	t.Helper()
	vm := idVM(t, r)
	edit(vm)
	require.NoError(t, r.Update(context.Background(), vm))
}

// sentKinds lists the kinds of the stub's sends, in order.
func sentKinds(stub *identityStub) []contracts.OperationKind {
	var out []contracts.OperationKind
	for _, p := range stub.sent() {
		out = append(out, p.Kind)
	}
	return out
}

// decodeSent decodes a prepared operation's request.
func decodeSent(t *testing.T, p contracts.PreparedOperation) proto.Message {
	t.Helper()
	m, err := decodePrepared(&p)
	require.NoError(t, err)
	return m
}

// honestReconfigure makes the fixture's Provider report the honest
// Reconfigure result, which a clustered resize needs.
func honestReconfigure(t *testing.T, r *VirtualMachineReconciler) {
	t.Helper()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	p.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{SupportsHonestReconfigure: true}
	require.NoError(t, r.Update(context.Background(), p))
}

// Each power state the spec can ask for is sent as a signed attempt of the
// matching kind, whose request carries the PowerRequest.op the kind fixes;
// nothing is sent unsigned, and the VM is Ready only after a Describe.
func TestIdentityPower_SignedAttemptPerKind(t *testing.T) {
	cases := []struct {
		desired  infravirtrigaudiov1beta1.PowerState
		observed string
		kind     contracts.OperationKind
		op       providerv1.PowerOp
	}{
		{infravirtrigaudiov1beta1.PowerStateOn, "Off", contracts.OperationKindPowerOn, providerv1.PowerOp_POWER_OP_ON},
		{infravirtrigaudiov1beta1.PowerStateOff, "On", contracts.OperationKindPowerOff, providerv1.PowerOp_POWER_OP_OFF},
		{infravirtrigaudiov1beta1.PowerStateOffGraceful, "On", contracts.OperationKindShutdownGraceful,
			providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			stub := newIdentityStub()
			r, req := createdIdentityVM(t, stub)
			stub.describeResp.PowerState = tc.observed
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Spec.PowerState = tc.desired })
			reconcileN(t, r, req, 1)

			vm := idVM(t, r)
			a := findAttempt(vm, tc.kind)
			require.NotNil(t, a)
			assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase)
			assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, a.Outcome)
			assert.Equal(t, int64(1), a.Attempt)
			assert.Equal(t, "host-alpha", a.HostID)
			assert.Equal(t, bindingIDOf(vm), a.BindingID)
			sends := stub.sent()
			require.Len(t, sends, 2)
			assert.Equal(t, tc.kind, sends[1].Kind)
			pr, ok := decodeSent(t, sends[1]).(*providerv1.PowerRequest)
			require.True(t, ok)
			assert.Equal(t, tc.op, pr.GetOp())
			assert.Equal(t, "vm-1", pr.GetId())
			assert.Equal(t, "uid-vm-id", pr.GetOwner().GetUid())
			assert.Empty(t, stub.powerRefs, "no unsigned Power")
			assert.Empty(t, intentSecrets(t, r))
			assert.NotEqual(t, k8s.ReasonReconcileSuccess, readyReason(vm), "Ready only after a Describe")
		})
	}
}

// Reboot and graceful shutdown are bound to their own PowerRequest.op.
func TestIdentityPower_RebootAndShutdownBoundToPowerOp(t *testing.T) {
	for kind, op := range map[contracts.OperationKind]providerv1.PowerOp{
		contracts.OperationKindReboot:           providerv1.PowerOp_POWER_OP_REBOOT,
		contracts.OperationKindShutdownGraceful: providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL,
	} {
		t.Run(string(kind), func(t *testing.T) {
			stub := newIdentityStub()
			r, _ := createdIdentityVM(t, stub)
			providerCR := &infravirtrigaudiov1beta1.Provider{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, providerCR))
			vm := idVM(t, r)
			ref, err := vmRefFor(vm, providerCR)
			require.NoError(t, err)
			_, err = r.startIdentityPowerOp(context.Background(), vm, providerCR, stub, ref, kind)
			require.NoError(t, err)
			sends := stub.sent()
			require.Len(t, sends, 2)
			assert.Equal(t, kind, sends[1].Kind)
			pr, ok := decodeSent(t, sends[1]).(*providerv1.PowerRequest)
			require.True(t, ok)
			assert.Equal(t, op, pr.GetOp())
			a := findAttempt(idVM(t, r), kind)
			require.NotNil(t, a)
			assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, a.Outcome)
		})
	}
	t.Run("a kind with no signed power path is refused", func(t *testing.T) {
		stub := newIdentityStub()
		r, _ := createdIdentityVM(t, stub)
		_, err := r.startIdentityPowerOp(context.Background(), idVM(t, r), nil, stub, contracts.VMRef{}, contracts.OperationKindReconfigure)
		require.ErrorIs(t, err, contracts.ErrOperationIdentityNotImplemented)
		assert.Len(t, stub.sent(), 1)
	})
}

// An asynchronous power attempt is polled until its task is over; a later
// request for the same kind is a new attempt (new op_id, next number), but
// not sooner than repeatGap after the last one Succeeded.
func TestIdentityPower_AsyncThenRepeatAfterGap(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	outcome := contracts.TaskOutcomeRunning
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind)}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) { return contracts.TaskStatus{Outcome: outcome}, nil }
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	reconcileN(t, r, req, 2)
	a := findAttempt(idVM(t, r), contracts.OperationKindPowerOn)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, a.Phase)
	assert.Len(t, stub.sent(), 2, "the attempt in flight is polled, not sent again")
	assert.Len(t, intentSecrets(t, r), 1)

	outcome = contracts.TaskOutcomeSucceeded
	reconcileN(t, r, req, 1)
	first := findAttempt(idVM(t, r), contracts.OperationKindPowerOn)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, first.Outcome)
	assert.Empty(t, intentSecrets(t, r))

	// The VM is still reported Off (the Provider lags, or it went off again):
	// no new attempt within the gap.
	reconcileN(t, r, req, 2)
	assert.Len(t, stub.sent(), 2, "no new attempt within the gap")

	r.clock = func() time.Time { return time.Now().Add(repeatGap + time.Second) }
	outcome = contracts.TaskOutcomeRunning
	reconcileN(t, r, req, 1)
	second := findAttempt(idVM(t, r), contracts.OperationKindPowerOn)
	require.NotNil(t, second)
	assert.Equal(t, int64(2), second.Attempt)
	assert.NotEqual(t, first.OpID, second.OpID)
	assert.Len(t, stub.sent(), 3)
}

// The outcome table is the Create's: a not-performed power attempt is
// followed by a new one after its backoff; a dirty failure holds.
func TestIdentityPower_OutcomeTable(t *testing.T) {
	t.Run("FailedClean then a new attempt after the backoff", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind) + "-" + string(rune('0'+p.Identity.Attempt))}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true, Error: "x"}, nil
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 3)
		assert.Equal(t, k8s.ReasonOperationRetryBackoff, readyReason(idVM(t, r)))
		assert.Len(t, stub.sent(), 2, "no new attempt during the backoff")
		r.clock = func() time.Time { return time.Now().Add(blockedRetryMax) }
		reconcileN(t, r, req, 1)
		assert.Len(t, stub.sent(), 3)
		assert.Equal(t, uint64(2), stub.sent()[2].Identity.Attempt)
	})
	t.Run("FailedDirty holds", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-power"}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedDirty, IsCompleted: true, Error: "x"}, nil
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 3)
		r.clock = func() time.Time { return time.Now().Add(blockedRetryMax) }
		reconcileN(t, r, req, 2)
		assert.Equal(t, k8s.ReasonOperationFailedDirty, readyReason(idVM(t, r)))
		assert.Len(t, stub.sent(), 2, "no new attempt after a dirty failure")
	})
	t.Run("UNKNOWN_TO_AGENT holds", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-power"}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 3)
		assert.Equal(t, k8s.ReasonOperationUnknownToAgent, readyReason(idVM(t, r)))
		assert.Len(t, stub.sent(), 2)
	})
	t.Run("a transport error re-sends the same attempt", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		fail := true
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			if fail {
				fail = false
				return contracts.OperationAnswer{}, errors.New("connection reset")
			}
			return contracts.OperationAnswer{}, nil
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 2)
		sends := stub.sent()
		require.Len(t, sends, 3)
		assert.Equal(t, sends[1].Identity.OpID, sends[2].Identity.OpID)
		assert.Equal(t, sends[1].RequestBytes, sends[2].RequestBytes)
		assert.Equal(t, sends[1].DigestSalt, sends[2].DigestSalt)
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded,
			findAttempt(idVM(t, r), contracts.OperationKindPowerOn).Outcome)
	})
}

// A tampered power intent (another op, another VM id) is never sent.
func TestIdentityPower_TamperedIntentHeld(t *testing.T) {
	cases := map[string]func(t *testing.T, r *VirtualMachineReconciler, s *corev1.Secret){
		"a REBOOT op under a PowerOn attempt (resealed)": func(t *testing.T, r *VirtualMachineReconciler, s *corev1.Secret) {
			b, err := proto.Marshal(&providerv1.PowerRequest{Id: "vm-1", Op: providerv1.PowerOp_POWER_OP_REBOOT, TargetHostId: "host-alpha",
				Owner: &providerv1.ObjectIdentity{Uid: "uid-vm-id"}})
			require.NoError(t, err)
			s.Data[intentKeyRequest] = b
			reseal(r, s)
		},
		"another VM id (resealed)": func(t *testing.T, r *VirtualMachineReconciler, s *corev1.Secret) {
			b, err := proto.Marshal(&providerv1.PowerRequest{Id: "vm-other", Op: providerv1.PowerOp_POWER_OP_ON, TargetHostId: "host-alpha",
				Owner: &providerv1.ObjectIdentity{Uid: "uid-vm-id"}})
			require.NoError(t, err)
			s.Data[intentKeyRequest] = b
			reseal(r, s)
		},
		"request bytes (unsealed)": func(t *testing.T, _ *VirtualMachineReconciler, s *corev1.Secret) {
			s.Data[intentKeyRequest] = append(s.Data[intentKeyRequest], 0x01)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			r, req := createdIdentityVM(t, stub)
			fail := true
			stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
				if fail {
					fail = false
					return contracts.OperationAnswer{}, errors.New("connection reset")
				}
				return contracts.OperationAnswer{}, nil
			}
			stub.describeResp.PowerState = "Off"
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
			})
			reconcileN(t, r, req, 1)
			secrets := intentSecrets(t, r)
			require.Len(t, secrets, 1)
			r.Recorder = record.NewFakeRecorder(10)
			tamper(t, r, &secrets[0])
			require.NoError(t, r.Update(context.Background(), &secrets[0]))
			reconcileN(t, r, req, 2)
			assert.Equal(t, k8s.ReasonOperationIntentTampered, readyReason(idVM(t, r)))
			assert.Len(t, stub.sent(), 2, "nothing is sent from a tampered intent")
		})
	}
}

// A resize is sent as a signed Reconfigure whose request carries the size
// asked for; that size, not the spec when the attempt ends, is what is
// recorded once it Succeeded.
func TestIdentityReconfigure_BindsRequestedResources(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	honestReconfigure(t, r)
	outcome := contracts.TaskOutcomeRunning
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "task-reconfigure", Honest: true}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) { return contracts.TaskStatus{Outcome: outcome}, nil }
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
	})
	reconcileN(t, r, req, 1)
	a := findAttempt(idVM(t, r), contracts.OperationKindReconfigure)
	require.NotNil(t, a)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, a.Phase)
	sends := stub.sent()
	require.Len(t, sends, 2)
	rr, ok := decodeSent(t, sends[1]).(*providerv1.ReconfigureRequest)
	require.True(t, ok)
	cpu, mem, err := requestedResources(rr)
	require.NoError(t, err)
	assert.Equal(t, int32(3), cpu)
	assert.Equal(t, int64(4096), mem)
	assert.Empty(t, stub.reconfigureRefs, "no unsigned Reconfigure")

	// The spec changes again while the attempt is in flight: nothing new is sent.
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4)}
	})
	reconcileN(t, r, req, 1)
	assert.Len(t, stub.sent(), 2)

	outcome = contracts.TaskOutcomeSucceeded
	reconcileN(t, r, req, 1)
	vm := idVM(t, r)
	require.NotNil(t, vm.Status.CurrentResources)
	assert.Equal(t, int32(3), *vm.Status.CurrentResources.CPU, "the size the attempt asked for")
	assert.Equal(t, int64(4096), *vm.Status.CurrentResources.MemoryMiB)

	// The newer size is a new attempt after the gap.
	r.clock = func() time.Time { return time.Now().Add(repeatGap + time.Second) }
	outcome = contracts.TaskOutcomeRunning
	reconcileN(t, r, req, 1)
	second := findAttempt(idVM(t, r), contracts.OperationKindReconfigure)
	assert.Equal(t, int64(2), second.Attempt)
	rr2, ok := decodeSent(t, stub.sent()[2]).(*providerv1.ReconfigureRequest)
	require.True(t, ok)
	cpu2, _, err := requestedResources(rr2)
	require.NoError(t, err)
	assert.Equal(t, int32(4), cpu2)
}

// A synchronous Reconfigure answer is recorded like the unsigned path's:
// applied, pending a restart, or not trusted without the honest marker.
func TestIdentityReconfigure_SyncAnswers(t *testing.T) {
	cases := map[string]struct {
		answer  contracts.OperationAnswer
		reason  string
		wantCPU int32
	}{
		"applied":          {contracts.OperationAnswer{Honest: true}, k8s.ReasonReconcileSuccess, 3},
		"restart required": {contracts.OperationAnswer{Honest: true, RestartRequired: true}, k8s.ReasonRestartRequired, 3},
		"no honest marker": {contracts.OperationAnswer{}, k8s.ReasonProviderLacksHonestReconfigure, 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			r, req := createdIdentityVM(t, stub)
			honestReconfigure(t, r)
			stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) { return tc.answer, nil }
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
			})
			reconcileN(t, r, req, 1)
			vm := idVM(t, r)
			a := findAttempt(vm, contracts.OperationKindReconfigure)
			require.NotNil(t, a)
			assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, a.Outcome)
			c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
			require.NotNil(t, c)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Equal(t, tc.wantCPU, *vm.Status.CurrentResources.CPU)
			assert.Empty(t, intentSecrets(t, r))
		})
	}
}

// A tampered Reconfigure intent (another size, resealed with the right key
// but naming no resources) is never sent.
func TestIdentityReconfigure_TamperedIntentHeld(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	honestReconfigure(t, r)
	fail := true
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		if fail {
			fail = false
			return contracts.OperationAnswer{}, errors.New("connection reset")
		}
		return contracts.OperationAnswer{Honest: true}, nil
	}
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
	})
	reconcileN(t, r, req, 1)
	secrets := intentSecrets(t, r)
	require.Len(t, secrets, 1)
	s := &secrets[0]
	desired, err := json.Marshal(contracts.CreateRequest{})
	require.NoError(t, err)
	b, err := proto.Marshal(&providerv1.ReconfigureRequest{Id: "vm-1", DesiredJson: string(desired), TargetHostId: "host-alpha",
		Owner: &providerv1.ObjectIdentity{Uid: "uid-vm-id"}})
	require.NoError(t, err)
	s.Data[intentKeyRequest] = b
	reseal(r, s)
	require.NoError(t, r.Update(context.Background(), s))
	r.Recorder = record.NewFakeRecorder(10)
	reconcileN(t, r, req, 2)
	assert.Equal(t, k8s.ReasonOperationIntentTampered, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 2)
}

// At most one attempt is in flight per VM across kinds: a resize requested
// while a power operation is in flight waits for it, and is then decided from
// the VM's state; a Delete waits for whatever is in flight and comes first
// after it.
func TestIdentity_SerialisesAcrossKinds(t *testing.T) {
	t.Run("a Reconfigure waits for a power attempt in flight", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		honestReconfigure(t, r)
		powerDone := false
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			if p.Kind == contracts.OperationKindPowerOff {
				return contracts.OperationAnswer{TaskRef: "task-off"}, nil
			}
			return contracts.OperationAnswer{Honest: true}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			if powerDone {
				return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded}, nil
			}
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		stub.describeResp.PowerState = "On"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		})
		reconcileN(t, r, req, 1)
		require.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOff}, sentKinds(stub))
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
		})
		reconcileN(t, r, req, 3)
		assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOff}, sentKinds(stub),
			"no Reconfigure while the power-off is in flight")
		assert.Nil(t, findAttempt(idVM(t, r), contracts.OperationKindReconfigure))

		powerDone = true
		stub.describeResp.PowerState = "Off"
		reconcileN(t, r, req, 2)
		assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOff,
			contracts.OperationKindReconfigure}, sentKinds(stub))
		assert.Equal(t, int32(3), *idVM(t, r).Status.CurrentResources.CPU)
	})
	t.Run("a Delete waits for a Reconfigure in flight, then comes first", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		honestReconfigure(t, r)
		rcDone := false
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			if p.Kind == contracts.OperationKindReconfigure {
				return contracts.OperationAnswer{TaskRef: "task-rc", Honest: true}, nil
			}
			return contracts.OperationAnswer{}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			if rcDone {
				return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded}, nil
			}
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
		})
		reconcileN(t, r, req, 1)
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 2)
		assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindReconfigure}, sentKinds(stub),
			"no Delete while the Reconfigure is in flight")
		rcDone = true
		reconcileN(t, r, req, 3)
		assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindReconfigure,
			contracts.OperationKindDelete}, sentKinds(stub), "the Delete comes first; no power operation for a VM being deleted")
		assert.True(t, vmGone(t, r))
	})
}

// For every Provider type that does not require operation identity, power
// and resize are the unsigned calls, as before: no marker, no signed
// operation.
func TestNonIdentityProviders_PowerAndResizeUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		typ      infravirtrigaudiov1beta1.ProviderType
		topology string
	}{
		{"vsphere", infravirtrigaudiov1beta1.ProviderTypeVSphere, ""},
		{"libvirt single", infravirtrigaudiov1beta1.ProviderTypeLibvirt, ""},
		{"libvirt cluster", infravirtrigaudiov1beta1.ProviderTypeLibvirt, infravirtrigaudiov1beta1.ProviderTopologyCluster},
		{"proxmox", infravirtrigaudiov1beta1.ProviderTypeProxmox, ""},
		{"mock", "mock", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newIdentityStub()
			stub.caps = contracts.Capabilities{SupportsListOwnerFilter: true}
			r, req := identityFixture(t, stub)
			p := &infravirtrigaudiov1beta1.Provider{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
			p.Spec.Type, p.Spec.Topology = tc.typ, tc.topology
			require.NoError(t, r.Update(context.Background(), p))
			honestReconfigure(t, r)
			reconcileN(t, r, req, 2)

			stub.describeResp.PowerState = "Off"
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
			})
			reconcileN(t, r, req, 1)
			assert.NotEmpty(t, stub.powerRefs, "the unsigned Power, as before")

			stub.describeResp.PowerState = "On"
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(1)}
			})
			reconcileN(t, r, req, 2)
			vm := idVM(t, r)
			assert.Empty(t, vm.Status.OperationAttempts)
			assert.Empty(t, stub.builds)
			assert.Empty(t, stub.sent())
			assert.Empty(t, intentSecrets(t, r))
		})
	}
}

// For a Provider that requires operation identity, an answer that finds no VM
// never releases the Delete finalizer: not an unsigned NotFound on the send,
// not a signed NO_RECORD of the Delete or of the VM's placement. The VM is
// held, the same Delete attempt is re-sent after the host signed that it has
// no record of it, and force-delete still releases it. A signed DELETED of
// the VM's placement (here by an earlier Delete of it on that host) releases
// it.
func TestIdentityDelete_AbsentNeverReleases(t *testing.T) {
	for name, absent := range map[string]error{
		"not found":    contracts.NewNotFoundError("no such VM", nil),
		"VM not owned": contracts.NewNotFoundError("not this VM's", contracts.ErrVMNotOwned),
	} {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			r, req := createdIdentityVM(t, stub)
			stub.describeResp = contracts.DescribeResponse{Exists: false}
			stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
				return contracts.OperationAnswer{}, absent
			}
			markDeleted(t, r, nil)
			reconcileN(t, r, req, 3)
			require.False(t, vmGone(t, r), "an absent VM does not confirm the Delete")
			vm := idVM(t, r)
			d := findAttempt(vm, contracts.OperationKindDelete)
			require.NotNil(t, d)
			assert.Equal(t, int64(1), d.Attempt, "no new Delete attempt")
			assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, d.Phase)
			sends := stub.sent()
			require.GreaterOrEqual(t, len(sends), 3)
			for _, p := range sends[1:] {
				assert.Equal(t, sends[1].Identity.OpID, p.Identity.OpID, "the same op_id is re-sent")
			}
			assert.Zero(t, stub.deletes, "no unsigned Delete")

			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			})
			reconcileN(t, r, req, 1)
			assert.True(t, vmGone(t, r), "force-delete releases it")
		})
	}
	t.Run("signed DELETED releases", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{}, contracts.NewNotFoundError("no such VM", nil)
		}
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 1)
		require.False(t, vmGone(t, r))
		stub.signed.vmDeleted = bytes.Repeat([]byte{0x5d}, 16)
		reconcileN(t, r, req, 1)
		assert.True(t, vmGone(t, r), "released on the host's signed DELETED of the placement")
	})
	t.Run("unsigned DELETED never releases", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{}, contracts.NewNotFoundError("no such VM", nil)
		}
		stub.signed.vmDeleted = bytes.Repeat([]byte{0x5d}, 16)
		stub.signed.unsigned = true
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 3)
		assert.False(t, vmGone(t, r))
	})
}

// A Delete task the host has no record of (UNKNOWN_TO_AGENT) holds; it is
// never read as "deleted".
func TestIdentityDelete_UnknownToAgentHolds(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	stub.describeResp = contracts.DescribeResponse{Exists: false}
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "del-task"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
	}
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 4)
	require.False(t, vmGone(t, r))
	assert.Equal(t, k8s.ReasonOperationUnknownToAgent, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 2, "no new Delete attempt")
}

// A VM whose Create was performed keeps its finalizer when its Provider is
// gone (nothing can confirm its Delete); one whose Create was never performed
// is released as before.
func TestIdentityDelete_ProviderGone(t *testing.T) {
	removeProvider := func(t *testing.T, r *VirtualMachineReconciler) {
		t.Helper()
		p := &infravirtrigaudiov1beta1.Provider{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
		p.Finalizers = nil
		require.NoError(t, r.Update(context.Background(), p))
		require.NoError(t, r.Delete(context.Background(), p))
	}
	t.Run("created: held", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		removeProvider(t, r)
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 2)
		require.False(t, vmGone(t, r))
		assert.Equal(t, k8s.ReasonOperationDeleteUnconfirmed, readyReason(idVM(t, r)))
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		})
		reconcileN(t, r, req, 1)
		assert.True(t, vmGone(t, r))
	})
	t.Run("never created: released", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		assert.False(t, identityDeleteUnconfirmed(idVM(t, r)))
		removeProvider(t, r)
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 1)
		assert.True(t, vmGone(t, r))
	})
}

// A Delete supersedes a held power attempt: the held attempt ends
// Terminal/Superseded, its intent Secret is kept as evidence, a Warning event
// records it, and the signed Delete is sent; the VM is released only once the
// Delete is confirmed.
func TestIdentityDelete_SupersedesHeldPowerAttempt(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	deleteDone := false
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind)}, nil
	}
	stub.onTask = func(ref string) (contracts.TaskStatus, error) {
		if ref == "task-Delete" {
			if deleteDone {
				return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded}, nil
			}
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
	}
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	reconcileN(t, r, req, 2)
	require.Equal(t, k8s.ReasonOperationUnknownToAgent, readyReason(idVM(t, r)), "the PowerOn is held")
	held := findAttempt(idVM(t, r), contracts.OperationKindPowerOn)
	require.NotNil(t, held)

	markDeleted(t, r, nil)
	reconcileN(t, r, req, 3)
	vm := idVM(t, r)
	p := findAttempt(vm, contracts.OperationKindPowerOn)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, p.Phase)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded, p.Outcome)
	d := findAttempt(vm, contracts.OperationKindDelete)
	require.NotNil(t, d)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, d.Phase)
	assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOn,
		contracts.OperationKindDelete}, sentKinds(stub))
	require.False(t, vmGone(t, r), "not released before the Delete is confirmed")
	names := map[string]bool{}
	for _, s := range intentSecrets(t, r) {
		names[s.Name] = true
	}
	assert.True(t, names[intentSecretName(held.OpID)], "the superseded attempt's intent Secret is kept as evidence")
	evs := drainEvents(rec)
	n := 0
	for _, e := range evs {
		if strings.Contains(e, eventReasonOperationSuperseded) {
			n++
			assert.Contains(t, e, held.OpID)
		}
	}
	assert.Equal(t, 1, n)
	assert.True(t, intentKeptAsEvidence(vm, p))
	r.cleanupTerminalIntents(context.Background(), vm, providerOf(t, r))
	names = map[string]bool{}
	for _, s := range intentSecrets(t, r) {
		names[s.Name] = true
	}
	assert.True(t, names[intentSecretName(held.OpID)], "terminal clean-up keeps it too")

	deleteDone = true
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r), "released once the Delete is confirmed")
}

// A held Create is never superseded: the Delete waits behind it, as before.
func TestIdentityDelete_HeldCreateIsNotSuperseded(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-" + string(p.Kind)}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
	}
	reconcileN(t, r, req, 2)
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 3)
	vm := idVM(t, r)
	assert.False(t, vmGone(t, r))
	assert.Equal(t, k8s.ReasonOperationUnknownToAgent, readyReason(vm))
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(vm, contracts.OperationKindCreate).Phase)
	assert.Nil(t, findAttempt(vm, contracts.OperationKindDelete))
	assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate}, sentKinds(stub))
}

// A PowerOff never supersedes a held PowerOn: it waits.
func TestIdentityPower_HeldPowerOnNotSupersededByPowerOff(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind)}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
	}
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	reconcileN(t, r, req, 2)
	stub.describeResp.PowerState = "On"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	})
	reconcileN(t, r, req, 3)
	assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOn}, sentKinds(stub))
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(idVM(t, r), contracts.OperationKindPowerOn).Phase)
}

// An asynchronous Reconfigure keeps its answer's flags on the Sent marker,
// under the task tag: an unmarked answer is not trusted when its task
// Succeeds, and an edited flag unbinds the task.
func TestIdentityReconfigure_AsyncFlagsKeptUnderTaskTag(t *testing.T) {
	setup := func(t *testing.T, honest bool) (*identityStub, *VirtualMachineReconciler, ctrl.Request, *contracts.TaskOutcome) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		honestReconfigure(t, r)
		outcome := contracts.TaskOutcomeRunning
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-rc", Honest: honest}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) { return contracts.TaskStatus{Outcome: outcome}, nil }
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(1)}
		})
		reconcileN(t, r, req, 1)
		return stub, r, req, &outcome
	}
	t.Run("an unmarked answer is recorded at the larger size", func(t *testing.T) {
		_, r, req, outcome := setup(t, false)
		a := findAttempt(idVM(t, r), contracts.OperationKindReconfigure)
		require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, a.Phase)
		assert.False(t, a.Honest)
		*outcome = contracts.TaskOutcomeSucceeded
		reconcileN(t, r, req, 1)
		vm := idVM(t, r)
		assert.Equal(t, int32(2), *vm.Status.CurrentResources.CPU, "a shrink without the honest marker is not trusted")
		assert.Equal(t, k8s.ReasonProviderLacksHonestReconfigure, k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)
	})
	t.Run("an honest answer is recorded at the requested size", func(t *testing.T) {
		_, r, req, outcome := setup(t, true)
		assert.True(t, findAttempt(idVM(t, r), contracts.OperationKindReconfigure).Honest)
		*outcome = contracts.TaskOutcomeSucceeded
		reconcileN(t, r, req, 1)
		assert.Equal(t, int32(1), *idVM(t, r).Status.CurrentResources.CPU)
	})
	t.Run("an edited flag unbinds the task", func(t *testing.T) {
		_, r, req, outcome := setup(t, false)
		vm := idVM(t, r)
		for i := range vm.Status.OperationAttempts {
			if vm.Status.OperationAttempts[i].Kind == infravirtrigaudiov1beta1.OperationAttemptKindReconfigure {
				vm.Status.OperationAttempts[i].Honest = true
			}
		}
		require.NoError(t, r.Status().Update(context.Background(), vm))
		*outcome = contracts.TaskOutcomeSucceeded
		reconcileN(t, r, req, 1)
		vm = idVM(t, r)
		assert.Equal(t, k8s.ReasonOperationStateLost, readyReason(vm))
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(vm, contracts.OperationKindReconfigure).Phase)
	})
}

// A FailedDirty power hold ends only with an acknowledgement naming that
// attempt's exact op_id; it is consumed (removed) with an event. It does
// nothing for another op_id, nor for a Create.
func TestIdentity_AcknowledgeFailedDirty(t *testing.T) {
	setAnnotation := func(t *testing.T, r *VirtualMachineReconciler, v string) {
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Annotations = map[string]string{acknowledgeOperationAnnotation: v}
		})
	}
	t.Run("power", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		rec := record.NewFakeRecorder(20)
		r.Recorder = rec
		dirty := true
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-" + string(rune('0'+p.Identity.Attempt))}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			if dirty {
				return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedDirty, IsCompleted: true, Error: "x"}, nil
			}
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded}, nil
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 3)
		require.Equal(t, k8s.ReasonOperationFailedDirty, readyReason(idVM(t, r)))
		first := findAttempt(idVM(t, r), contracts.OperationKindPowerOn)

		setAnnotation(t, r, "00000000000000000000000000000000")
		reconcileN(t, r, req, 2)
		assert.Equal(t, k8s.ReasonOperationFailedDirty, readyReason(idVM(t, r)), "another op_id does nothing")
		assert.Len(t, stub.sent(), 2)
		assert.Equal(t, "00000000000000000000000000000000", idVM(t, r).Annotations[acknowledgeOperationAnnotation])

		dirty = false
		setAnnotation(t, r, first.OpID)
		reconcileN(t, r, req, 2)
		vm := idVM(t, r)
		assert.NotContains(t, vm.Annotations, acknowledgeOperationAnnotation, "consumed")
		require.Len(t, stub.sent(), 3)
		second := findAttempt(vm, contracts.OperationKindPowerOn)
		assert.Equal(t, int64(2), second.Attempt)
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, second.Outcome)
		found := false
		for _, e := range drainEvents(rec) {
			found = found || strings.Contains(e, eventReasonOperationAcknowledged)
		}
		assert.True(t, found, "the acknowledgement is recorded")
	})
	t.Run("a Create is not acknowledged", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-create"}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedDirty, IsCompleted: true, Error: "x"}, nil
		}
		reconcileN(t, r, req, 2)
		c := findAttempt(idVM(t, r), contracts.OperationKindCreate)
		require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedDirty, c.Outcome)
		setAnnotation(t, r, c.OpID)
		reconcileN(t, r, req, 2)
		vm := idVM(t, r)
		assert.Equal(t, k8s.ReasonOperationFailedDirty, readyReason(vm))
		assert.Equal(t, c.OpID, vm.Annotations[acknowledgeOperationAnnotation], "not consumed")
		assert.Len(t, stub.sent(), 1)
	})
}

// The gap after a power or Reconfigure attempt that Succeeded doubles with
// each attempt of the kind (15 s to 5 min), and a lastTransitionTime in the
// future never makes it longer than the maximum.
func TestAttemptRetryWait_DoublesAndClamps(t *testing.T) {
	now := time.Now()
	at := func(attempt int64, last time.Time) *infravirtrigaudiov1beta1.OperationAttempt {
		return &infravirtrigaudiov1beta1.OperationAttempt{Attempt: attempt, LastTransitionTime: metav1.NewTime(last)}
	}
	assert.Equal(t, 15*time.Second, attemptRetryWait(at(1, now), now))
	assert.Equal(t, 30*time.Second, attemptRetryWait(at(2, now), now))
	assert.Equal(t, 60*time.Second, attemptRetryWait(at(3, now), now))
	assert.Equal(t, blockedRetryMax, attemptRetryWait(at(20, now), now))
	assert.Zero(t, attemptRetryWait(at(20, now.Add(-time.Hour)), now), "long after the last attempt, no wait")
	assert.Equal(t, blockedRetryMax, attemptRetryWait(at(20, now.Add(1000*time.Hour)), now), "a future timestamp counts as now")
	assert.Equal(t, 15*time.Second, attemptRetryWait(at(1, now.Add(1000*time.Hour)), now))
}

// A power attempt's send that meets an unavailable host, or a VM missing on
// (or not owned by) its host, is held as the unsigned path holds it; the same
// attempt is re-sent later with the same op_id.
func TestIdentityPower_HostLevelSendAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"host unavailable": {contracts.NewHostUnavailableError("host down", nil), k8s.ReasonHostUnavailable},
		"VM not found":     {contracts.NewNotFoundError("no such VM", nil), k8s.ReasonVMMissingOnHost},
		"VM not owned":     {contracts.NewNotFoundError("not this VM's", contracts.ErrVMNotOwned), k8s.ReasonVMMissingOnHost},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			r, req := createdIdentityVM(t, stub)
			stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
				return contracts.OperationAnswer{}, tc.err
			}
			stub.describeResp.PowerState = "Off"
			setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
			})
			res := reconcileN(t, r, req, 1)
			vm := idVM(t, r)
			assert.Equal(t, tc.reason, readyReason(vm))
			assert.Greater(t, res.RequeueAfter, blockedRetryMin, "not the 15 s cadence")
			a := findAttempt(vm, contracts.OperationKindPowerOn)
			require.NotNil(t, a)
			assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase, "the attempt stays, to be re-sent")
			reconcileN(t, r, req, 1)
			sends := stub.sent()
			require.Len(t, sends, 3, "re-sent after the host signed it has no record of it")
			assert.Equal(t, sends[1].Identity.OpID, sends[2].Identity.OpID)
		})
	}
}

// A signed Reconfigure carries no cloud-init user data or metadata, so
// neither reaches its intent Secret or grant; a stored one that does is held
// as tampered.
func TestIdentityReconfigure_NoCloudInit(t *testing.T) {
	assert.Nil(t, reconfigureProjection(contracts.CreateRequest{UserData: &contracts.UserData{CloudInitData: "secret"},
		MetaData: &contracts.MetaData{}}).UserData)
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	honestReconfigure(t, r)
	fail := true
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		if fail {
			fail = false
			return contracts.OperationAnswer{}, errors.New("connection reset")
		}
		return contracts.OperationAnswer{Honest: true}, nil
	}
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
	})
	reconcileN(t, r, req, 1)
	sends := stub.sent()
	require.Len(t, sends, 2)
	rr, ok := decodeSent(t, sends[1]).(*providerv1.ReconfigureRequest)
	require.True(t, ok)
	require.NoError(t, checkReconfigureProjection(rr))
	assert.NotContains(t, rr.GetDesiredJson(), "CloudInitData")

	secrets := intentSecrets(t, r)
	require.Len(t, secrets, 1)
	s := &secrets[0]
	desired, err := json.Marshal(contracts.CreateRequest{Class: contracts.VMClass{CPU: 3, MemoryMiB: 4096},
		UserData: &contracts.UserData{CloudInitData: "#cloud-config"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&providerv1.ReconfigureRequest{Id: "vm-1", DesiredJson: string(desired), TargetHostId: "host-alpha",
		Owner: &providerv1.ObjectIdentity{Uid: "uid-vm-id"}})
	require.NoError(t, err)
	s.Data[intentKeyRequest] = b
	reseal(r, s)
	require.NoError(t, r.Update(context.Background(), s))
	r.Recorder = record.NewFakeRecorder(10)
	reconcileN(t, r, req, 1)
	assert.Equal(t, k8s.ReasonOperationIntentTampered, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 2)
}

// The unsigned task fields are never polled for a VM of a Provider that
// requires operation identity: one found set is cleared.
func TestIdentity_LegacyTaskRefsIgnored(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	polled := 0
	stub.IsTaskCompleteFn = func(context.Context, string) (bool, error) { polled++; return true, nil }
	vm := idVM(t, r)
	vm.Status.LastTaskRef, vm.Status.ReconfigureTaskRef = "forged-task", "forged-rc-task"
	require.NoError(t, r.Status().Update(context.Background(), vm))
	reconcileN(t, r, req, 1)
	vm = idVM(t, r)
	assert.Zero(t, polled, "never polled")
	assert.Empty(t, vm.Status.LastTaskRef)
	assert.Empty(t, vm.Status.ReconfigureTaskRef)
}

// The repeat gap doubles only over consecutive successes of a kind that new
// attempts follow within repeatWindow; a request after the window waits not
// at all and starts again at repeatGap.
func TestIdentityPower_RepeatGapStreakWithinWindow(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	now := time.Now()
	r.clock = func() time.Time { return now }
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	powerOns := func() int {
		n := 0
		for _, k := range sentKinds(stub) {
			if k == contracts.OperationKindPowerOn {
				n++
			}
		}
		return n
	}
	reconcileN(t, r, req, 1)
	require.Equal(t, 1, powerOns(), "the first PowerOn")

	// The guest keeps powering itself off: the gaps are 15 s, then 30 s.
	now = now.Add(repeatGap - time.Second)
	reconcileN(t, r, req, 1)
	assert.Equal(t, 1, powerOns())
	now = now.Add(2 * time.Second)
	reconcileN(t, r, req, 1)
	require.Equal(t, 2, powerOns())
	now = now.Add(repeatGap + time.Second)
	reconcileN(t, r, req, 1)
	assert.Equal(t, 2, powerOns(), "the second quick repeat waits 30 s")
	now = now.Add(repeatGap)
	reconcileN(t, r, req, 1)
	require.Equal(t, 3, powerOns())

	// Long after the last success: no wait, and the gap is back to 15 s.
	now = now.Add(repeatWindow + time.Minute)
	reconcileN(t, r, req, 1)
	require.Equal(t, 4, powerOns(), "after the window, at once")
	now = now.Add(repeatGap + time.Second)
	reconcileN(t, r, req, 1)
	assert.Equal(t, 5, powerOns(), "the streak started again")
	assert.Equal(t, int64(5), findAttempt(idVM(t, r), contracts.OperationKindPowerOn).Attempt)

	assert.Equal(t, repeatGap, repeatGapFor(0))
	assert.Equal(t, 2*repeatGap, repeatGapFor(1))
	assert.Equal(t, blockedRetryMax, repeatGapFor(50))
}

// A Sent power attempt reported running past sentAttemptDeadline is held as
// OperationOutcomeUnknown, so a Delete supersedes it; a Create is never held
// for running long.
func TestIdentity_SentAttemptDeadline(t *testing.T) {
	t.Run("power: held, then superseded by the Delete", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := createdIdentityVM(t, stub)
		stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{TaskRef: "task-" + string(p.Kind)}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		stub.describeResp.PowerState = "Off"
		setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		})
		reconcileN(t, r, req, 2)
		assert.Equal(t, k8s.ReasonOperationInProgress, readyReason(idVM(t, r)), "within the deadline")

		r.clock = func() time.Time { return time.Now().Add(sentAttemptDeadline + time.Minute) }
		reconcileN(t, r, req, 1)
		assert.Equal(t, k8s.ReasonOperationOutcomeUnknown, readyReason(idVM(t, r)))
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(idVM(t, r), contracts.OperationKindPowerOn).Phase)

		markDeleted(t, r, nil)
		reconcileN(t, r, req, 3)
		vm := idVM(t, r)
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded, findAttempt(vm, contracts.OperationKindPowerOn).Outcome)
		assert.Equal(t, []contracts.OperationKind{contracts.OperationKindCreate, contracts.OperationKindPowerOn,
			contracts.OperationKindDelete}, sentKinds(stub), "the signed Delete is sent")
	})
	t.Run("a future transition time counts as now", func(t *testing.T) {
		r := &VirtualMachineReconciler{}
		a := &infravirtrigaudiov1beta1.OperationAttempt{Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn,
			Phase: infravirtrigaudiov1beta1.OperationAttemptPhaseSent, LastTransitionTime: metav1.NewTime(time.Now().Add(1000 * time.Hour))}
		assert.False(t, r.runningPastDeadline(a))
		a.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * sentAttemptDeadline))
		assert.True(t, r.runningPastDeadline(a))
		a.Kind = infravirtrigaudiov1beta1.OperationAttemptKindDelete
		assert.False(t, r.runningPastDeadline(a), "never a Delete")
	})
	t.Run("a Create in flight past the deadline is unchanged", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-create"}, nil
		}
		stub.onTask = func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		reconcileN(t, r, req, 1)
		r.clock = func() time.Time { return time.Now().Add(sentAttemptDeadline + time.Hour) }
		reconcileN(t, r, req, 2)
		vm := idVM(t, r)
		assert.Equal(t, k8s.ReasonOperationInProgress, readyReason(vm))
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, findAttempt(vm, contracts.OperationKindCreate).Phase)
		markDeleted(t, r, nil)
		reconcileN(t, r, req, 2)
		assert.Nil(t, findAttempt(idVM(t, r), contracts.OperationKindDelete), "the Delete still waits for the Create")
	})
}
