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
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the operation-identity lifecycle of the VirtualMachine
// controller against a scripted Provider client (identityStub) and the fake
// Kubernetes client. The same lifecycle runs against the instrumented fake
// Provider over real gRPC, on a real API server, in the envtest specs of
// virtualmachine_operation_identity_envtest_test.go (development builds).

// identityStub is a Provider client that sends signed operations: it records
// every prepared and sent operation and answers from scripts.
type identityStub struct {
	routingProvider

	mu      sync.Mutex
	caps    contracts.Capabilities
	builds  []contracts.PreparedOperation
	sends   []contracts.PreparedOperation
	onSend  func(p contracts.PreparedOperation) (contracts.OperationAnswer, error)
	onTask  func(ref string) (contracts.TaskStatus, error)
	creates int // unsigned Creates (must stay 0)
	deletes int // unsigned Deletes (must stay 0)
}

func newIdentityStub() *identityStub {
	s := &identityStub{caps: contracts.Capabilities{SupportsOperationIdentity: true, RequiresOperationIdentity: true,
		SupportsListOwnerFilter: true}}
	s.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "Off"}
	return s
}

func (s *identityStub) GetCapabilities(context.Context) (contracts.Capabilities, error) { return s.caps, nil }

func (s *identityStub) ListVMsForOwner(context.Context, contracts.OwnerFilter) (contracts.VMList, error) {
	return contracts.VMList{OwnerFilterApplied: true}, nil
}

func (s *identityStub) BuildOperation(_ context.Context, id contracts.OperationAttemptIdentity,
	req contracts.OperationRequest) (contracts.PreparedOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	salt, err := operation.NewDigestSalt()
	if err != nil {
		return contracts.PreparedOperation{}, err
	}
	body, _ := json.Marshal(map[string]string{"kind": string(req.Kind), "name": req.Spec.Name, "id": req.VM.ID, "host": id.HostID})
	p := contracts.PreparedOperation{Kind: req.Kind, Identity: id, RequestBytes: body, DigestSalt: salt}
	s.builds = append(s.builds, p)
	return p, nil
}

func (s *identityStub) SendOperation(_ context.Context, p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
	s.mu.Lock()
	s.sends = append(s.sends, p)
	on := s.onSend
	s.mu.Unlock()
	if on != nil {
		return on(p)
	}
	if p.Kind == contracts.OperationKindCreate {
		return contracts.OperationAnswer{ID: "vm-1"}, nil
	}
	return contracts.OperationAnswer{}, nil
}

func (s *identityStub) TaskStatus(_ context.Context, ref string) (contracts.TaskStatus, error) {
	if s.onTask != nil {
		return s.onTask(ref)
	}
	return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded, IsCompleted: true}, nil
}

func (s *identityStub) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	s.creates++
	return s.routingProvider.Create(ctx, req)
}

func (s *identityStub) Delete(ctx context.Context, ref contracts.VMRef) (string, error) {
	s.deletes++
	return s.routingProvider.Delete(ctx, ref)
}

func (s *identityStub) sent() []contracts.PreparedOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contracts.PreparedOperation(nil), s.sends...)
}

// identityFixture is a clustered cloudhypervisor Provider with one host
// (host-alpha) and an enrolment for it, and an unbound VM with its finalizer.
func identityFixture(t *testing.T, stub contracts.Provider) (*VirtualMachineReconciler, ctrl.Request) {
	t.Helper()
	vm := clusterVM("vm-id", clusteredNS, "prov-cluster")
	vm.UID = "uid-vm-id"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
	providerCR.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	providerCR.UID = "uid-provider"
	pool := hostPoolCR("pool-a", clusteredNS, providerCR.Name)
	host := readyHost("host-alpha", clusteredNS, pool.Name, providerCR.Name)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: stub}, vm, providerCR, pool, host,
		smallVMClass(clusteredNS), minimalVMImage(clusteredNS))
	holder := make([]byte, operation.IDLen)
	holder[0] = 1
	enrol := make([]byte, operation.IDLen)
	enrol[0] = 2
	r.OperationEnrolments = operation.StaticEnrolments{
		Epoch: operation.Epoch{Counter: 2, Holder: holder},
		Hosts: map[string]map[string][]byte{clusteredNS + "/prov-cluster": {"host-alpha": enrol}},
	}
	return r, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clusteredNS, Name: vm.Name}}
}

func reconcileN(t *testing.T, r *VirtualMachineReconciler, req ctrl.Request, n int) ctrl.Result {
	t.Helper()
	var res ctrl.Result
	for i := 0; i < n; i++ {
		var err error
		res, err = r.Reconcile(context.Background(), req)
		require.NoError(t, err)
	}
	return res
}

func idVM(t *testing.T, r *VirtualMachineReconciler) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	return getVM(t, r, "vm-id")
}

func intentSecrets(t *testing.T, r *VirtualMachineReconciler) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	require.NoError(t, r.List(context.Background(), &list, client.MatchingLabels{operationIntentLabel: operationIntentLabelTrue}))
	return list.Items
}

func grpcReason(code codes.Code, reason string) error {
	st, _ := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain})
	return st.Err()
}

// providerErr wraps a gRPC status the way the transport client does.
func providerErr(code codes.Code, reason string) error {
	return &contracts.ProviderError{Type: contracts.ErrorTypeRetryable, Message: "refused", Cause: grpcReason(code, reason)}
}

func TestOperationHintMismatch(t *testing.T) {
	both := contracts.Capabilities{SupportsOperationIdentity: true, RequiresOperationIdentity: true}
	assert.Empty(t, operationHintMismatch(operation.ProtectionIdentity, both))
	assert.NotEmpty(t, operationHintMismatch(operation.ProtectionIdentity, contracts.Capabilities{SupportsOperationIdentity: true}))
	assert.NotEmpty(t, operationHintMismatch(operation.ProtectionIdentity, contracts.Capabilities{}), "never downgraded to unsigned")
	fenced := both
	fenced.SupportsFencing = true
	assert.NotEmpty(t, operationHintMismatch(operation.ProtectionIdentity, fenced), "any fencing gate is a mismatch")
	assert.NotEmpty(t, operationHintMismatch(operation.ProtectionNone, contracts.Capabilities{RequiresFencing: true}))
	assert.Empty(t, operationHintMismatch(operation.ProtectionNone, contracts.Capabilities{}))
	assert.NotEmpty(t, operationHintMismatch(operation.ProtectionNone, contracts.Capabilities{RequiresOperationIdentity: true}))
}

func TestSetOperationIdentityReadiness(t *testing.T) {
	both := contracts.Capabilities{SupportsOperationIdentity: true, RequiresOperationIdentity: true}
	ch := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}}
	reason := func(p *infravirtrigaudiov1beta1.Provider) string {
		if c := k8s.GetCondition(p.Status.Conditions, k8s.ConditionReady); c != nil {
			return string(c.Status) + "/" + c.Reason
		}
		return ""
	}
	setOperationIdentityReadiness(ch, both, true)
	assert.Equal(t, "True/"+k8s.ReasonOperationIdentityMatched, reason(ch))
	setOperationIdentityReadiness(ch, both, false)
	assert.Equal(t, "False/"+k8s.ReasonOperationSignerUnavailable, reason(ch))
	setOperationIdentityReadiness(ch, contracts.Capabilities{}, true)
	assert.Equal(t, "False/"+k8s.ReasonOperationIdentityMismatch, reason(ch))

	lv := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeLibvirt}}
	setOperationIdentityReadiness(lv, contracts.Capabilities{}, true)
	assert.Empty(t, reason(lv), "a Provider that requires nothing and advertises nothing gets no Ready condition")
	setOperationIdentityReadiness(lv, contracts.Capabilities{SupportsFencing: true}, true)
	assert.Equal(t, "False/"+k8s.ReasonOperationIdentityMismatch, reason(lv))
	setOperationIdentityReadiness(lv, contracts.Capabilities{}, true)
	assert.Empty(t, reason(lv), "removed once it no longer advertises a gate")
}

// A synchronous Create: Prepared marker and intent Secret before the send,
// then Terminal/Succeeded with the id and the promoted placement in one
// write, and the Secret deleted.
func TestIdentityCreate_SyncSuccess(t *testing.T) {
	stub := newIdentityStub()
	var atSend *infravirtrigaudiov1beta1.VirtualMachine
	var secretsAtSend int
	r, req := identityFixture(t, stub)
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		atSend = idVM(t, r)
		secretsAtSend = len(intentSecrets(t, r))
		return contracts.OperationAnswer{ID: "vm-1"}, nil
	}
	reconcileN(t, r, req, 1)

	require.Len(t, stub.sent(), 1)
	require.NotNil(t, atSend)
	a := findAttempt(atSend, contracts.OperationKindCreate)
	require.NotNil(t, a, "the marker is written before the send")
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, a.Phase)
	assert.Equal(t, int64(1), a.Attempt, "attempts count from 1")
	assert.Equal(t, hex.EncodeToString(stub.sent()[0].Identity.OpID), a.OpID)
	assert.Equal(t, "host-alpha", a.HostID)
	assert.Equal(t, 1, secretsAtSend, "the intent Secret is written before the send")
	assert.NotEmpty(t, bindingIDOf(atSend), "the binding id is written with the first marker")
	assert.Equal(t, a.BindingID, bindingIDOf(atSend))

	vm := idVM(t, r)
	done := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, done)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, done.Phase)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, done.Outcome)
	assert.Equal(t, "vm-1", vm.Status.ID)
	assert.Equal(t, "host-alpha", vm.Status.Placement.Host)
	assert.Empty(t, vm.Status.Placement.PendingHost)
	assert.Equal(t, a.BindingID, vm.Status.Placement.BindingID, "the binding id survives promotion")
	assert.Empty(t, intentSecrets(t, r), "the intent Secret is deleted once Terminal")
	assert.Zero(t, stub.creates, "no unsigned Create")

	p := stub.sent()[0]
	assert.Equal(t, "uid-provider", p.Identity.ProviderUID)
	assert.Equal(t, "uid-vm-id", p.Identity.VMUID)
	assert.Equal(t, uint64(1), p.Identity.Attempt)
	assert.Equal(t, uint64(2), p.Identity.Epoch.Counter)
}

// The intent Secret carries the request and salt, owned by the Provider, in
// its namespace, with the VM's labels; never anything in the VM's status.
func TestIdentityCreate_IntentSecretShape(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	var secret corev1.Secret
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		s := intentSecrets(t, r)
		require.Len(t, s, 1)
		secret = s[0]
		return contracts.OperationAnswer{}, errors.New("connection reset")
	}
	reconcileN(t, r, req, 1)
	p := stub.sent()[0]
	assert.Equal(t, intentSecretName(hex.EncodeToString(p.Identity.OpID)), secret.Name)
	assert.Equal(t, clusteredNS, secret.Namespace)
	assert.Equal(t, operationIntentSecretType, secret.Type)
	assert.Equal(t, p.RequestBytes, secret.Data[intentKeyRequest])
	assert.Equal(t, p.DigestSalt, secret.Data[intentKeySalt])
	assert.Equal(t, "uid-vm-id", secret.Labels[operationIntentVMUIDLabel])
	require.Len(t, secret.OwnerReferences, 1)
	assert.Equal(t, "Provider", secret.OwnerReferences[0].Kind)
	assert.Equal(t, types.UID("uid-provider"), secret.OwnerReferences[0].UID)

	raw, err := json.Marshal(idVM(t, r).Status)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), hex.EncodeToString(p.DigestSalt), "the salt never reaches the status")
	assert.NotContains(t, string(raw), string(p.RequestBytes), "the request never reaches the status")
}

// A transport error leaves the attempt Prepared; the next reconcile re-sends
// the stored bytes with the same op_id and salt, never a new attempt.
func TestIdentityCreate_TransportErrorResendsSameAttempt(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	calls := 0
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		calls++
		if calls == 1 {
			return contracts.OperationAnswer{}, errors.New("connection reset")
		}
		return contracts.OperationAnswer{ID: "vm-1"}, nil
	}
	reconcileN(t, r, req, 1)
	a := findAttempt(idVM(t, r), contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, a.Phase)

	reconcileN(t, r, req, 1)
	sends := stub.sent()
	require.Len(t, sends, 2)
	assert.Len(t, stub.builds, 1, "built once")
	assert.Equal(t, sends[0].Identity.OpID, sends[1].Identity.OpID)
	assert.Equal(t, sends[0].RequestBytes, sends[1].RequestBytes)
	assert.Equal(t, sends[0].DigestSalt, sends[1].DigestSalt)
	assert.Equal(t, "vm-1", idVM(t, r).Status.ID)
}

// An asynchronous Create: Sent with the task, the answer id kept in the
// intent Secret, then Terminal with the id once the task Succeeded.
func TestIdentityCreate_AsyncTask(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{ID: "vm-async", TaskRef: "task-1"}, nil
	}
	running := true
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		if running {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded, IsCompleted: true}, nil
	}
	reconcileN(t, r, req, 1)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, a.Phase)
	assert.Equal(t, "task-1", a.TaskRef)
	assert.Empty(t, vm.Status.ID, "no id until the task Succeeded")
	assert.Equal(t, k8s.ReasonOperationInProgress, readyReason(vm))

	reconcileN(t, r, req, 1)
	assert.Len(t, stub.sent(), 1, "a Sent attempt is polled, never re-sent")
	running = false
	reconcileN(t, r, req, 1)
	vm = idVM(t, r)
	assert.Equal(t, "vm-async", vm.Status.ID)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, findAttempt(vm, contracts.OperationKindCreate).Outcome)
	assert.Empty(t, intentSecrets(t, r))
}

// Clean not-performed outcomes end the attempt; a new attempt (new op_id,
// attempt 2) follows only after the backoff. The outcome is the Provider's
// unsigned claim: a lying Provider can make the manager mint a second
// attempt (a stated residual until host-signed answers exist).
func TestIdentityCreate_NotPerformedThenNewAttemptAfterBackoff(t *testing.T) {
	for _, outcome := range []contracts.TaskOutcome{contracts.TaskOutcomeRetired, contracts.TaskOutcomeFailedClean, contracts.TaskOutcomeCancelled} {
		t.Run(string(outcome), func(t *testing.T) {
			stub := newIdentityStub()
			r, req := identityFixture(t, stub)
			now := time.Now()
			r.clock = func() time.Time { return now }
			stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
				return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-" + hex.EncodeToString(p.Identity.OpID)}, nil
			}
			stub.onTask = func(string) (contracts.TaskStatus, error) {
				return contracts.TaskStatus{Outcome: outcome, IsCompleted: true, Error: "no", OutcomeReason: "never_received"}, nil
			}
			reconcileN(t, r, req, 2)
			vm := idVM(t, r)
			a := findAttempt(vm, contracts.OperationKindCreate)
			require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase)
			assert.Equal(t, "never_received", a.OutcomeReason)
			assert.Equal(t, k8s.ReasonOperationRetryBackoff, readyReason(vm))
			assert.Empty(t, intentSecrets(t, r))

			reconcileN(t, r, req, 1)
			assert.Len(t, stub.sent(), 1, "no new attempt during the backoff")

			now = now.Add(blockedRetryMin + time.Second)
			stub.onTask = func(string) (contracts.TaskStatus, error) {
				return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
			}
			reconcileN(t, r, req, 1)
			sends := stub.sent()
			require.Len(t, sends, 2)
			assert.NotEqual(t, sends[0].Identity.OpID, sends[1].Identity.OpID, "a new attempt has a new op_id")
			assert.Equal(t, uint64(2), sends[1].Identity.Attempt)
			assert.Equal(t, sends[0].Identity.BindingID, sends[1].Identity.BindingID, "same placement, same binding id")
			assert.Equal(t, int64(2), findAttempt(idVM(t, r), contracts.OperationKindCreate).Attempt)
		})
	}
}

// Outcomes that prove nothing, or something bad, hold: no new attempt.
func TestIdentityCreate_Holds(t *testing.T) {
	cases := map[string]struct {
		task   func(string) (contracts.TaskStatus, error)
		reason string
		phase  infravirtrigaudiov1beta1.OperationAttemptPhase
	}{
		"FailedDirty": {func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedDirty, IsCompleted: true, Error: "x"}, nil
		}, k8s.ReasonOperationFailedDirty, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal},
		"Unknown": {func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeUnknown}, nil
		}, k8s.ReasonOperationOutcomeUnknown, infravirtrigaudiov1beta1.OperationAttemptPhaseSent},
		"UnknownToAgent": {func(string) (contracts.TaskStatus, error) {
			return contracts.TaskStatus{}, contracts.NewNotFoundError("no such task", errors.Join(contracts.ErrUnknownToAgent))
		}, k8s.ReasonOperationUnknownToAgent, infravirtrigaudiov1beta1.OperationAttemptPhaseSent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			r, req := identityFixture(t, stub)
			r.clock = func() time.Time { return time.Now().Add(time.Hour) }
			stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
				return contracts.OperationAnswer{ID: "vm-1", TaskRef: "task-1"}, nil
			}
			stub.onTask = tc.task
			reconcileN(t, r, req, 4)
			vm := idVM(t, r)
			assert.Equal(t, tc.reason, readyReason(vm))
			assert.Equal(t, tc.phase, findAttempt(vm, contracts.OperationKindCreate).Phase)
			assert.Len(t, stub.sent(), 1, "never a new attempt")
			assert.Empty(t, vm.Status.ID)
		})
	}
}

// Refusals of a send.
func TestIdentityCreate_SendRefusals(t *testing.T) {
	t.Run("PROVIDER_MISMATCH holds", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{}, providerErr(codes.FailedPrecondition, contracts.ProviderMismatchReason)
		}
		reconcileN(t, r, req, 1)
		vm := idVM(t, r)
		assert.Equal(t, k8s.ReasonOperationProviderMismatch, readyReason(vm))
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, findAttempt(vm, contracts.OperationKindCreate).Phase)
	})
	t.Run("GRANT_MISMATCH on the first delivery ends the attempt", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			return contracts.OperationAnswer{}, providerErr(codes.FailedPrecondition, contracts.GrantMismatchReason)
		}
		reconcileN(t, r, req, 1)
		a := findAttempt(idVM(t, r), contracts.OperationKindCreate)
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected, a.Outcome)
		assert.Equal(t, contracts.GrantMismatchReason, a.OutcomeReason)
		assert.Empty(t, intentSecrets(t, r))
	})
	t.Run("GRANT_MISMATCH on a re-delivery holds", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		calls := 0
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			calls++
			if calls == 1 {
				return contracts.OperationAnswer{}, errors.New("deadline exceeded")
			}
			return contracts.OperationAnswer{}, providerErr(codes.FailedPrecondition, contracts.GrantMismatchReason)
		}
		reconcileN(t, r, req, 2)
		vm := idVM(t, r)
		assert.Equal(t, k8s.ReasonOperationOutcomeUnknown, readyReason(vm))
		assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, findAttempt(vm, contracts.OperationKindCreate).Phase)
	})
	t.Run("VM_BUSY re-sends the same attempt", func(t *testing.T) {
		stub := newIdentityStub()
		r, req := identityFixture(t, stub)
		calls := 0
		stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
			calls++
			if calls == 1 {
				return contracts.OperationAnswer{}, providerErr(codes.Aborted, contracts.VMBusyReason)
			}
			return contracts.OperationAnswer{ID: "vm-1"}, nil
		}
		reconcileN(t, r, req, 2)
		sends := stub.sent()
		require.Len(t, sends, 2)
		assert.Equal(t, sends[0].Identity.OpID, sends[1].Identity.OpID)
		assert.Equal(t, "vm-1", idVM(t, r).Status.ID)
	})
}

// A VM whose Create marker was removed (status loss) is held: no new op_id.
func TestIdentityCreate_StatusLossHolds(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{}, errors.New("connection reset")
	}
	reconcileN(t, r, req, 1)
	vm := idVM(t, r)
	vm.Status.OperationAttempts = nil
	require.NoError(t, r.Status().Update(context.Background(), vm))

	reconcileN(t, r, req, 2)
	assert.Equal(t, k8s.ReasonOperationStateLost, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 1, "no new attempt after the marker was lost")
	assert.Len(t, stub.builds, 1)
}

// A marker naming a missing intent Secret (an etcd restore) is held.
func TestIdentityCreate_IntentLostHolds(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{}, errors.New("connection reset")
	}
	reconcileN(t, r, req, 1)
	for _, s := range intentSecrets(t, r) {
		require.NoError(t, r.Delete(context.Background(), &s))
	}
	reconcileN(t, r, req, 2)
	assert.Equal(t, k8s.ReasonOperationIntentLost, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 1, "nothing is re-sent without the exact bytes")
}

// A Provider that does not advertise what its type requires is held: nothing
// is built or sent, and nothing is sent unsigned instead.
func TestIdentityCreate_HintMismatchHolds(t *testing.T) {
	for name, caps := range map[string]contracts.Capabilities{
		"no identity": {SupportsListOwnerFilter: true},
		"fencing": {SupportsOperationIdentity: true, RequiresOperationIdentity: true, RequiresFencing: true,
			SupportsListOwnerFilter: true},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newIdentityStub()
			stub.caps = caps
			r, req := identityFixture(t, stub)
			reconcileN(t, r, req, 2)
			vm := idVM(t, r)
			assert.Equal(t, k8s.ReasonOperationIdentityMismatch, readyReason(vm))
			assert.Empty(t, stub.builds)
			assert.Empty(t, stub.sent())
			assert.Zero(t, stub.creates)
			assert.Empty(t, vm.Status.OperationAttempts)
		})
	}
}

// No enrolment for the host, or no signing client: held, nothing sent.
func TestIdentityCreate_NoEnrolmentOrSender(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	r.OperationEnrolments = nil
	reconcileN(t, r, req, 2)
	assert.Equal(t, k8s.ReasonOperationEnrolmentUnavailable, readyReason(idVM(t, r)))
	assert.Empty(t, stub.sent())

	plain := &routingProvider{}
	r2, req2 := identityFixture(t, plain)
	reconcileN(t, r2, req2, 2)
	assert.Equal(t, k8s.ReasonOperationSignerUnavailable, readyReason(getVM(t, r2, "vm-id")))
	assert.Empty(t, plain.createReqs, "an unsigned client is never used for an identity Provider")
}

// A marker write that loses its test precondition sends nothing, and the
// unused intent Secret is removed.
func TestIdentityCreate_MarkerConflictSendsNothing(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	r.Client = &markerRacingClient{Client: r.Client}
	reconcileN(t, r, req, 1)
	assert.Empty(t, stub.sent(), "a marker write that failed its precondition sends nothing")
	assert.Empty(t, intentSecrets(t, r), "the unused intent Secret is removed")
}

// markerRacingClient writes a foreign marker right before the first JSON
// Patch of the status, so its test precondition fails.
type markerRacingClient struct {
	client.Client
	raced bool
}

func (m *markerRacingClient) Status() client.SubResourceWriter {
	return &racingStatusWriter{SubResourceWriter: m.Client.Status(), m: m}
}

type racingStatusWriter struct {
	client.SubResourceWriter
	m *markerRacingClient
}

func (w *racingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if patch.Type() == types.JSONPatchType && !w.m.raced {
		w.m.raced = true
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := w.m.Client.Get(ctx, client.ObjectKeyFromObject(obj), vm); err != nil {
			return err
		}
		vm.Status.OperationAttempts = []infravirtrigaudiov1beta1.OperationAttempt{{
			Kind: infravirtrigaudiov1beta1.OperationAttemptKindCreate, Attempt: 1, OpID: "00000000000000000000000000000001",
			Phase: infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, LastTransitionTime: metav1.Now(),
		}}
		if err := w.m.Client.Status().Update(ctx, vm); err != nil {
			return err
		}
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// createdIdentityVM runs a synchronous Create to completion.
func createdIdentityVM(t *testing.T, stub *identityStub) (*VirtualMachineReconciler, ctrl.Request) {
	t.Helper()
	r, req := identityFixture(t, stub)
	reconcileN(t, r, req, 1)
	require.Equal(t, "vm-1", idVM(t, r).Status.ID)
	return r, req
}

func markDeleted(t *testing.T, r *VirtualMachineReconciler, annotations map[string]string) {
	t.Helper()
	vm := idVM(t, r)
	for k, v := range annotations {
		if vm.Annotations == nil {
			vm.Annotations = map[string]string{}
		}
		vm.Annotations[k] = v
	}
	require.NoError(t, r.Update(context.Background(), vm))
	require.NoError(t, r.Delete(context.Background(), vm))
}

func vmGone(t *testing.T, r *VirtualMachineReconciler) bool {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "vm-id"}, vm)
	return err != nil
}

// Delete: the finalizer is released only once the signed Delete Succeeded.
func TestIdentityDelete_ReleasesOnlyOnSucceeded(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	outcome := contracts.TaskOutcomeRunning
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "del-task"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) { return contracts.TaskStatus{Outcome: outcome}, nil }
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 2)
	require.False(t, vmGone(t, r), "a Delete in flight keeps the finalizer")
	d := findAttempt(idVM(t, r), contracts.OperationKindDelete)
	require.NotNil(t, d)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseSent, d.Phase)
	assert.Zero(t, stub.deletes, "no unsigned Delete")

	outcome = contracts.TaskOutcomeSucceeded
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r), "released once the Delete Succeeded")
	assert.Empty(t, intentSecrets(t, r))
}

// A Delete that failed dirty holds the finalizer; force-delete releases it.
func TestIdentityDelete_FailedDirtyHoldsUntilForce(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "del-task"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedDirty, IsCompleted: true, Error: "x"}, nil
	}
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 3)
	require.False(t, vmGone(t, r))
	assert.Equal(t, k8s.ReasonOperationFailedDirty, readyReason(idVM(t, r)))
	assert.Len(t, stub.sent(), 2, "one Create, one Delete: no new Delete attempt")

	vm := idVM(t, r)
	vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(context.Background(), vm))
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r), "force-delete releases a held identity delete")
}

// A VM whose Create was never performed is released without a Delete.
func TestIdentityDelete_NothingCreatedReleasesWithoutDelete(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{}, providerErr(codes.FailedPrecondition, contracts.GrantUnauthenticatedReason)
	}
	reconcileN(t, r, req, 1)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeRejected,
		findAttempt(idVM(t, r), contracts.OperationKindCreate).Outcome)
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r))
	assert.Len(t, stub.sent(), 1, "no Delete was sent")
}

// A Delete waits for the Create in flight (at most one operation per VM).
func TestIdentityDelete_WaitsForCreateInFlight(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		if p.Kind == contracts.OperationKindCreate {
			return contracts.OperationAnswer{ID: "vm-1", TaskRef: "create-task"}, nil
		}
		return contracts.OperationAnswer{}, nil
	}
	createDone := false
	stub.onTask = func(ref string) (contracts.TaskStatus, error) {
		if !createDone {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded, IsCompleted: true}, nil
	}
	reconcileN(t, r, req, 1)
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 2)
	assert.Len(t, stub.sent(), 1, "no Delete while the Create is in flight")
	createDone = true
	reconcileN(t, r, req, 3)
	assert.True(t, vmGone(t, r))
	sends := stub.sent()
	require.Len(t, sends, 2)
	assert.Equal(t, contracts.OperationKindDelete, sends[1].Kind)
}

// Orphan-on-delete releases without any provider call, as for every Provider.
func TestIdentityDelete_OrphanOnDeleteUnchanged(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	markDeleted(t, r, map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"})
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r))
	assert.Len(t, stub.sent(), 1, "only the Create")
}

// For every Provider type that does not require operation identity, the
// reconcile is unchanged: no marker, no intent Secret, no signed operation.
func TestNonIdentityProviders_NoMarkersNoSignedOperations(t *testing.T) {
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
			reconcileN(t, r, req, 2)
			vm := idVM(t, r)
			assert.Empty(t, vm.Status.OperationAttempts)
			assert.Empty(t, bindingIDOf(vm))
			assert.Empty(t, stub.builds)
			assert.Empty(t, stub.sent())
			assert.Equal(t, 1, stub.creates, "the unsigned Create, as before")
			assert.Empty(t, intentSecrets(t, r))

			markDeleted(t, r, nil)
			reconcileN(t, r, req, 1)
			assert.Empty(t, stub.sent())
			assert.Equal(t, 1, stub.deletes, "the unsigned Delete, as before")
		})
	}
}

// A release build has no signer: a cloudhypervisor Provider forced past
// admission is never resolved, the VM reports OperationSignerUnavailable, and
// no RPC is sent.
func TestIdentity_NoSignerFailsClosed(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	r.RemoteResolver = &stubResolver{err: operation.ErrNoSigner}
	reconcileN(t, r, req, 2)
	vm := idVM(t, r)
	assert.Equal(t, k8s.ReasonOperationSignerUnavailable, readyReason(vm))
	assert.Empty(t, stub.sent())
	assert.Empty(t, vm.Status.OperationAttempts)
}

// The sweeper removes intent Secrets nothing needs, after the grace period.
func TestSweepOperationIntents(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{}, errors.New("connection reset")
	}
	reconcileN(t, r, req, 1) // a Prepared attempt and its Secret
	provider := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, provider))
	ctx := context.Background()
	mk := func(name, vmName, vmUID string, owned bool) {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: clusteredNS,
				Labels:      map[string]string{operationIntentLabel: operationIntentLabelTrue, operationIntentVMUIDLabel: vmUID},
				Annotations: map[string]string{operationIntentVMNamespaceAnnotation: clusteredNS, operationIntentVMNameAnnotation: vmName}},
			Type: operationIntentSecretType,
		}
		if owned {
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "infra.virtrigaud.io/v1beta1", Kind: "Provider",
				Name: provider.Name, UID: provider.UID, Controller: boolTrue()}}
		}
		require.NoError(t, r.Create(ctx, s))
	}
	mk(intentSecretName("000000000000000000000000000000aa"), "vm-id", "uid-vm-id", true) // orphan of this VM
	mk(intentSecretName("000000000000000000000000000000bb"), "gone", "uid-gone", true)   // its VM is gone
	mk(intentSecretName("000000000000000000000000000000cc"), "gone", "uid-gone", false)  // not owned: never touched

	// The fake client stores no creation time: the Secrets were "created" at
	// the zero time.
	var created time.Time
	n, err := sweepOperationIntents(ctx, r.Client, r.Client, provider, created.Add(operationIntentSweepGrace/2))
	require.NoError(t, err)
	assert.Zero(t, n, "nothing younger than the grace period is swept")

	n, err = sweepOperationIntents(ctx, r.Client, r.Client, provider, created.Add(operationIntentSweepGrace+time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	names := map[string]bool{}
	for _, s := range intentSecrets(t, r) {
		names[s.Name] = true
	}
	a := findAttempt(idVM(t, r), contracts.OperationKindCreate)
	assert.True(t, names[intentSecretName(a.OpID)], "the Secret of the attempt in flight is kept")
	assert.True(t, names[intentSecretName("000000000000000000000000000000cc")], "a Secret the Provider does not own is kept")
	assert.Len(t, names, 2)
}

// boolTrue is a pointer to true.
func boolTrue() *bool { b := true; return &b }
