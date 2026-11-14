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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// userDataCanary is cloud-init user data that must never be kept once a
// Create is over.
var userDataCanary = []byte("#cloud-config\npassword: REDACTION-CANARY-7f3a")

// secretsByPrefix lists the intent Secrets whose name starts with prefix.
func secretsByPrefix(t *testing.T, r *VirtualMachineReconciler, prefix string) []corev1.Secret {
	t.Helper()
	var out []corev1.Secret
	for _, s := range intentSecrets(t, r) {
		if strings.HasPrefix(s.Name, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// redactedNotPerformedCreate runs a Create carrying user data that its host signs
// FAILED_CLEAN, and returns the VM's not-performed Create marker.
func redactedNotPerformedCreate(t *testing.T) (*identityStub, *VirtualMachineReconciler, ctrl.Request, *infravirtrigaudiov1beta1.OperationAttempt) {
	t.Helper()
	stub := newIdentityStub()
	stub.userData = userDataCanary
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "create-task"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true, OutcomeReason: "no_capacity"}, nil
	}
	reconcileN(t, r, req, 1)
	c := findAttempt(idVM(t, r), contracts.OperationKindCreate)
	require.NotNil(t, c)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeFailedClean, c.Outcome)
	return stub, r, req, c
}

// A Create that went Terminal not performed keeps only a redacted intent: no
// request bytes, no user data; and that is enough to release the VM without
// a Delete once its host signs the outcome again.
func TestIntentRedaction_EndToEnd(t *testing.T) {
	stub, r, req, _ := redactedNotPerformedCreate(t)
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix), "the full intent is deleted")
	red := secretsByPrefix(t, r, redactedIntentSecretPrefix)
	require.Len(t, red, 1)
	_, hasRequest := red[0].Data[intentKeyRequest]
	assert.False(t, hasRequest, "the redacted intent holds no request bytes")
	for k, v := range red[0].Data {
		assert.False(t, bytes.Contains(v, userDataCanary), "data key %s carries the user data", k)
		assert.False(t, bytes.Contains(v, []byte("REDACTION-CANARY")), "data key %s carries the user data", k)
	}
	assert.Len(t, red[0].Data[intentKeyDigest], 32)

	markDeleted(t, r, nil)
	reconcileN(t, r, req, 1)
	assert.True(t, vmGone(t, r), "released on the host's signed FAILED_CLEAN, asked from the redacted intent")
	assert.Len(t, stub.sent(), 1, "no Delete")
	deleted, _, err := sweepOperationIntents(context.Background(), r.Client, r.Client, providerOf(t, r), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "the sweeper removes the redacted intent once its VM is gone")
	assert.Empty(t, intentSecrets(t, r))
}

// The redacted intent's MAC is verified: an edited field, or its content put
// at the full intent's name, is refused.
func TestIntentRedaction_MACVerified(t *testing.T) {
	_, r, _, c := redactedNotPerformedCreate(t)
	vm := idVM(t, r)
	ns := clusteredNS
	ref, found, err := r.loadRedactedIntent(context.Background(), vm, c.OpID, ns)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, ref.Digest, 32)

	red := secretsByPrefix(t, r, redactedIntentSecretPrefix)[0]
	recreate := func(s *corev1.Secret) {
		t.Helper()
		_ = r.Delete(context.Background(), s.DeepCopy())
		s.ResourceVersion, s.UID = "", ""
		require.NoError(t, r.Create(context.Background(), s))
	}
	for name, edit := range map[string]func(s *corev1.Secret){
		"digest":  func(s *corev1.Secret) { s.Data[intentKeyDigest][0] ^= 1 },
		"salt":    func(s *corev1.Secret) { s.Data[intentKeySalt][0] ^= 1 },
		"request": func(s *corev1.Secret) { s.Data[intentKeyRequest] = []byte("x") },
		"VM name": func(s *corev1.Secret) { s.Annotations[operationIntentVMNameAnnotation] = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			s := red.DeepCopy()
			edit(s)
			recreate(s)
			_, _, err := r.loadRedactedIntent(context.Background(), vm, c.OpID, ns)
			require.ErrorIs(t, err, errIntentTampered)
			recreate(red.DeepCopy())
		})
	}
	// Its content under the full intent's name never authenticates as a full
	// intent (another MAC domain).
	full := red.DeepCopy()
	full.Name, full.ResourceVersion, full.UID = intentSecretName(c.OpID), "", ""
	require.NoError(t, r.Create(context.Background(), full))
	_, _, err = r.loadIntent(context.Background(), vm, c.OpID, ns)
	require.ErrorIs(t, err, errIntentTampered)
}

// The intent digest is the same from both forms, and binds a signed answer
// either way.
func TestIntentRedaction_DigestBothForms(t *testing.T) {
	stub := newIdentityStub()
	stub.userData = userDataCanary
	_, r, _, _ := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	p, found, err := r.loadIntent(context.Background(), vm, a.OpID, clusteredNS)
	require.NoError(t, err)
	require.True(t, found)
	fullRef := refOf(p)
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	redRef, found, err := r.loadPollIntent(context.Background(), vm, a.OpID, clusteredNS)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, fullRef.Digest, redRef.Digest)
	assert.Equal(t, fullRef.Identity, redRef.Identity)
	answer := &providerv1.OperationAnswer{OpId: p.Identity.OpID, OpEnrolmentId: p.Identity.EnrolmentID,
		State: providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN, Kind: int32(wireKinds[p.Kind]),
		Attempt: p.Identity.Attempt, VmUid: p.Identity.VMUID, BindingId: p.Identity.BindingID, IntentDigest: fullRef.Digest}
	assert.True(t, bindsTo(answer, fullRef))
	assert.True(t, bindsTo(answer, redRef))
	answer.IntentDigest = bytes.Repeat([]byte{9}, 32)
	assert.False(t, bindsTo(answer, redRef), "another digest does not bind")
}

// preparedWithIntentStub is preparedWithIntent with a given stub: a Create
// attempt in flight whose first delivery failed.
func preparedWithIntentStub(t *testing.T, stub *identityStub) (*identityStub, *VirtualMachineReconciler, ctrl.Request, *corev1.Secret) {
	t.Helper()
	r, req := identityFixture(t, stub)
	fail := true
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		if fail {
			fail = false
			return contracts.OperationAnswer{}, errors.New("connection reset")
		}
		return contracts.OperationAnswer{ID: "vm-1"}, nil
	}
	reconcileN(t, r, req, 1)
	secrets := intentSecrets(t, r)
	require.Len(t, secrets, 1)
	return stub, r, req, &secrets[0]
}

// A redacted intent can never be sent: an attempt in flight whose full
// intent is gone (only a redacted one remains) is held as OperationIntentLost
// when its host signs NO_RECORD, and nothing is sent; an empty request is
// never sent either.
func TestIntentRedaction_ResendFailsClosed(t *testing.T) {
	stub := newIdentityStub()
	stub, r, req, _ := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	sends := len(stub.sent())
	reconcileN(t, r, req, 2)
	assert.Len(t, stub.sent(), sends, "nothing is sent from a redacted intent")
	assert.Equal(t, k8s.ReasonOperationIntentLost, readyReason(idVM(t, r)))

	ia := identityAttempt{providerCR: providerOf(t, r), sender: stub, reader: stub, result: &attemptResult{}}
	step, _, err := r.sendAttempt(context.Background(), idVM(t, r), ia, a, contracts.PreparedOperation{Kind: contracts.OperationKindCreate}, false)
	require.NoError(t, err)
	assert.Equal(t, stepHeld, step)
	assert.Len(t, stub.sent(), sends, "an empty request is never sent")
}

// providerOf reads the fixture's Provider.
func providerOf(t *testing.T, r *VirtualMachineReconciler) *infravirtrigaudiov1beta1.Provider {
	t.Helper()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	return p
}

// The redaction is idempotent and finishes an interrupted one: run twice, or
// after a crash left both Secrets, it ends with exactly the redacted Secret.
func TestIntentRedaction_Idempotent(t *testing.T) {
	stub := newIdentityStub()
	_, r, _, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	full := s.DeepCopy()
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix))
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)

	// A crash between writing the redacted Secret and deleting the full one.
	full.ResourceVersion, full.UID = "", ""
	require.NoError(t, r.Create(context.Background(), full))
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix), "the interrupted redaction is finished")
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)
}

// conflictingDeleteClient answers the first Delete of a Secret with a
// conflict.
type conflictingDeleteClient struct {
	client.Client
	conflicts int
}

func (c *conflictingDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*corev1.Secret); ok && c.conflicts > 0 {
		c.conflicts--
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("modified"))
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// A conflict deleting the full intent is retried with a fresh read.
func TestIntentRedaction_ConflictRetried(t *testing.T) {
	stub := newIdentityStub()
	_, r, _, _ := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	r.Client = &conflictingDeleteClient{Client: r.Client, conflicts: 2}
	require.NoError(t, r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID))
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix))
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)
}

// The redaction of a Create that went Terminal is finished by the next
// reconcile when its first try failed.
func TestIntentRedaction_RetriedOnReconcile(t *testing.T) {
	stub := newIdentityStub()
	stub.userData = userDataCanary
	r, req := identityFixture(t, stub)
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "create-task"}, nil
	}
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true}, nil
	}
	base := r.Client
	r.Client = &conflictingDeleteClient{Client: base, conflicts: 100}
	reconcileN(t, r, req, 1)
	require.NotEmpty(t, secretsByPrefix(t, r, operationIntentSecretPrefix), "the delete failed: the full intent is still there")
	r.Client = base
	reconcileN(t, r, req, 1)
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix), "the next reconcile finished the redaction")
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)
}

// swapClient replaces the stored full intent Secret right after its first
// read (a principal with Secret write access racing the redaction), and
// counts the reads of intent Secrets.
type swapClient struct {
	client.Client
	swap func()
	// only, when set, limits the counted reads to the Secrets of this op_id.
	only  string
	mu    sync.Mutex
	done  bool
	reads int
}

func (c *swapClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*corev1.Secret); ok && strings.HasPrefix(key.Name, "vrtg-op") {
		c.mu.Lock()
		if c.only == "" || strings.HasSuffix(key.Name, c.only) {
			c.reads++
		}
		first := !c.done && c.swap != nil && strings.HasPrefix(key.Name, operationIntentSecretPrefix)
		c.done = c.done || first
		c.mu.Unlock()
		if first && err == nil {
			c.swap()
		}
	}
	return err
}

func (c *swapClient) secretReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// A full intent swapped between its authenticated read and the redaction is
// neither the source of the redacted Secret nor deleted: the redacted Secret
// is built from the authenticated read alone (its identity and owner), and
// the planted Secret, which does not authenticate, is kept as evidence.
func TestIntentRedaction_SwapBetweenReadAndUse(t *testing.T) {
	stub := newIdentityStub()
	_, r, _, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	base := r.Client
	sc := &swapClient{Client: base}
	sc.swap = func() {
		planted := &corev1.Secret{}
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(s), planted))
		var ident intentIdentity
		require.NoError(t, json.Unmarshal(planted.Data[intentKeyIdentity], &ident))
		ident.Identity.EnrolmentID = bytes.Repeat([]byte{0x66}, 16)
		planted.Data[intentKeyIdentity], _ = json.Marshal(ident)
		planted.OwnerReferences = nil
		require.NoError(t, base.Update(context.Background(), planted))
	}
	r.Client, r.APIReader = sc, sc
	err := r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID)
	require.ErrorIs(t, err, errIntentTampered, "the re-read Secret does not authenticate")
	r.Client, r.APIReader = base, nil

	full := secretsByPrefix(t, r, operationIntentSecretPrefix)
	require.Len(t, full, 1, "the planted Secret is kept as evidence")
	assert.Nil(t, full[0].OwnerReferences, "it is the planted one")
	red := secretsByPrefix(t, r, redactedIntentSecretPrefix)
	require.Len(t, red, 1)
	require.Len(t, red[0].OwnerReferences, 1)
	assert.Equal(t, identityProviderUID, red[0].OwnerReferences[0].UID, "the redacted Secret's owner comes from the Provider")
	ref, found, err := r.loadRedactedIntent(context.Background(), vm, a.OpID, s.Namespace)
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqual(t, bytes.Repeat([]byte{0x66}, 16), ref.Identity.EnrolmentID, "the identity comes from the authenticated read")
}

// A Secret already at the redacted intent's name that is not this attempt's
// blocks the redaction: the full intent is kept and a Warning event names
// both Secrets, once.
func TestIntentRedaction_BlockedIsAnnounced(t *testing.T) {
	stub := newIdentityStub()
	stub.userData = userDataCanary
	r, req := identityFixture(t, stub)
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "create-task"}, nil
	}
	running := true
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		if running {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true}, nil
	}
	reconcileN(t, r, req, 1)
	a := findAttempt(idVM(t, r), contracts.OperationKindCreate)
	full := secretsByPrefix(t, r, operationIntentSecretPrefix)[0]
	junk := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: redactedIntentName(a.OpID), Namespace: full.Namespace,
		Labels: full.Labels, Annotations: full.Annotations}, Type: operationIntentSecretType,
		Data: map[string][]byte{intentKeyDigest: bytes.Repeat([]byte{1}, 32)}}
	require.NoError(t, r.Create(context.Background(), junk))
	running = false
	reconcileN(t, r, req, 3)
	assert.Len(t, secretsByPrefix(t, r, operationIntentSecretPrefix), 1, "the full intent is kept")
	warnings := 0
	for len(rec.Events) > 0 {
		ev := <-rec.Events
		if strings.Contains(ev, eventReasonIntentRedactionBlocked) {
			warnings++
			assert.Contains(t, ev, intentSecretName(a.OpID))
			assert.Contains(t, ev, redactedIntentName(a.OpID))
			assert.NotContains(t, ev, "cloud-config")
		}
	}
	assert.Equal(t, 1, warnings, "announced once")

	// The operator removes the wrong Secret; the next reconcile redacts.
	require.NoError(t, r.Delete(context.Background(), junk))
	reconcileN(t, r, req, 1)
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix))
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)
}

// A not-performed Create whose intent this process saw redacted costs no
// Secret read on later reconciles, also past the sweep window (the VM's
// later attempts, with other op_ids, are not counted); a restarted manager
// checks it once.
func TestIntentRedaction_SettledVMCostsNoReads(t *testing.T) {
	_, r, req, c := redactedNotPerformedCreate(t)
	now := c.LastTransitionTime.Add(time.Second) // within the backoff: c stays the latest Create
	r.clock = func() time.Time { return now }
	base := r.Client
	sc := &swapClient{Client: base, only: c.OpID}
	r.Client, r.APIReader = sc, sc
	reconcileN(t, r, req, 3)
	assert.Zero(t, sc.secretReads(), "no intent Secret is read once the redaction is known done")

	r.redactions = sync.Map{} // a restarted manager
	reconcileN(t, r, req, 3)
	assert.Equal(t, 1, sc.secretReads(), "checked once after a restart, then not again")

	now = now.Add(operationIntentSweepGrace + time.Hour)
	reconcileN(t, r, req, 3)
	assert.Equal(t, 1, sc.secretReads(), "nothing more past the sweep window")
}

// An intent under a predecessor enrolment is held for the release without a
// Delete, even when the host signs FAILED_CLEAN for it.
func TestIntentRedaction_PredecessorIntentHeld(t *testing.T) {
	stub, r, req, _ := redactedNotPerformedCreate(t)
	p := providerOf(t, r)
	old := stub.signed.host
	h2 := newTestHost(t, p, "host-alpha", bytes.Repeat([]byte{0x45}, 16), old.trust.Enrolment.Epoch)
	h2.trust.Predecessors = [][]byte{old.trust.Enrolment.EnrolmentID}
	trust, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	trust.mu.Lock()
	trust.hosts["host-alpha"] = h2
	trust.mu.Unlock()
	stub.signed.host = h2
	stub.signed.predecessors = [][]byte{old.trust.Enrolment.EnrolmentID}
	markDeleted(t, r, nil)
	reconcileN(t, r, req, 2)
	assert.False(t, vmGone(t, r), "a predecessor's not-performed Create does not release the VM")
	vm := idVM(t, r)
	assert.Equal(t, k8s.ReasonOperationStateLost, readyReason(vm))
	c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, msgCreateNotPerformedUnconfirmed, c.Message, "held because the not-performed outcome is not signed again")
}

// redactionEntries counts the in-memory redaction entries.
func redactionEntries(r *VirtualMachineReconciler) int {
	n := 0
	r.redactions.Range(func(any, any) bool { n++; return true })
	return n
}

// The in-memory redaction state is one entry per VM, whatever the number of
// its op_ids, and is dropped once the VM is gone.
func TestIntentRedaction_StateBoundedPerVM(t *testing.T) {
	_, r, req, _ := redactedNotPerformedCreate(t)
	vm := idVM(t, r)
	p := providerOf(t, r)
	for i := 0; i < 50; i++ {
		a := &infravirtrigaudiov1beta1.OperationAttempt{Kind: infravirtrigaudiov1beta1.OperationAttemptKindCreate,
			OpID: strings.Repeat("0", 30) + string(rune('a'+i%26)) + string(rune('a'+i/26))}
		r.ensureRedacted(context.Background(), p, vm, a)
	}
	assert.Equal(t, 1, redactionEntries(r), "one entry per VM")
	vm.Finalizers = nil
	require.NoError(t, r.Update(context.Background(), vm))
	require.NoError(t, r.Delete(context.Background(), idVM(t, r)))
	reconcileN(t, r, req, 1)
	assert.Zero(t, redactionEntries(r), "dropped once the VM is gone")
}

// A full intent held by a finalizer is not deleted by its delete: the
// redaction is not done, a Warning says it is stuck, it is tried again, and
// it completes once the finalizer is removed.
func TestIntentRedaction_FinalizerOnFullIntent(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "create-task"}, nil
	}
	running := true
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		if running {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true}, nil
	}
	reconcileN(t, r, req, 1)
	full := secretsByPrefix(t, r, operationIntentSecretPrefix)[0]
	full.Finalizers = []string{"example.test/hold"}
	require.NoError(t, r.Update(context.Background(), &full))
	running = false
	reconcileN(t, r, req, 3)
	stuck := secretsByPrefix(t, r, operationIntentSecretPrefix)
	require.Len(t, stuck, 1, "the finalizer keeps the full intent")
	assert.NotNil(t, stuck[0].DeletionTimestamp)
	v, ok := r.redactions.Load(client.ObjectKeyFromObject(idVM(t, r)).String())
	require.True(t, ok)
	st, ok := v.(redactionState)
	require.True(t, ok)
	assert.False(t, st.done, "a stuck full intent is not done")
	warnings := 0
	for len(rec.Events) > 0 {
		if ev := <-rec.Events; strings.Contains(ev, eventReasonIntentRedactionBlocked) {
			warnings++
			assert.Contains(t, ev, "finalizers")
			assert.Contains(t, ev, full.Name)
		}
	}
	assert.Equal(t, 1, warnings)

	stuck[0].Finalizers = nil
	require.NoError(t, r.Update(context.Background(), &stuck[0]))
	reconcileN(t, r, req, 1)
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix))
	v, _ = r.redactions.Load(client.ObjectKeyFromObject(idVM(t, r)).String())
	st, ok = v.(redactionState)
	require.True(t, ok)
	assert.True(t, st.done)
	assert.False(t, st.warned, "a successful redaction clears the warning")
}

// uidPreconditionClient enforces a Delete's UID precondition as the API
// server does (the fake client does not): a Secret whose UID differs is a
// conflict.
type uidPreconditionClient struct{ *swapClient }

func (c uidPreconditionClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	do := &client.DeleteOptions{}
	do.ApplyOptions(opts)
	if do.Preconditions != nil && do.Preconditions.UID != nil {
		cur := &corev1.Secret{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), cur); err == nil && cur.UID != *do.Preconditions.UID {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("UID precondition failed"))
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// A swap by delete and re-create (a new UID) between the authenticated read
// and the delete is caught by the UID precondition: the re-created Secret,
// which does not authenticate, is kept.
func TestIntentRedaction_SwapByRecreate(t *testing.T) {
	stub := newIdentityStub()
	_, r, _, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	base := r.Client
	sc := &swapClient{Client: base}
	sc.swap = func() {
		planted := &corev1.Secret{}
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(s), planted))
		require.NoError(t, base.Delete(context.Background(), planted.DeepCopy()))
		original := planted.UID
		planted.ResourceVersion, planted.UID = "", types.UID("recreated-"+string(original))
		planted.Data[intentKeySalt] = bytes.Repeat([]byte{7}, 32)
		require.NoError(t, base.Create(context.Background(), planted))
	}
	uc := uidPreconditionClient{sc}
	r.Client, r.APIReader = uc, uc
	err := r.redactIntent(context.Background(), providerOf(t, r), vm, a.OpID)
	require.ErrorIs(t, err, errIntentTampered)
	r.Client, r.APIReader = base, nil
	full := secretsByPrefix(t, r, operationIntentSecretPrefix)
	require.Len(t, full, 1, "the re-created Secret is kept as evidence")
	assert.Equal(t, bytes.Repeat([]byte{7}, 32), full[0].Data[intentKeySalt])
}

// An authentic redacted Secret for the same op_id that carries another digest
// blocks the redaction: the full intent is kept and announced once.
func TestIntentRedaction_BlockedByMismatch(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	rec := record.NewFakeRecorder(50)
	r.Recorder = rec
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		return contracts.OperationAnswer{TaskRef: "create-task"}, nil
	}
	running := true
	stub.onTask = func(string) (contracts.TaskStatus, error) {
		if running {
			return contracts.TaskStatus{Outcome: contracts.TaskOutcomeRunning}, nil
		}
		return contracts.TaskStatus{Outcome: contracts.TaskOutcomeFailedClean, IsCompleted: true}, nil
	}
	reconcileN(t, r, req, 1)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	p, found, err := r.loadIntent(context.Background(), vm, a.OpID, clusteredNS)
	require.NoError(t, err)
	require.True(t, found)
	p.DigestSalt = bytes.Repeat([]byte{8}, 32) // same op_id, another digest
	other, err := buildRedactedIntentSecret(providerOf(t, r), vm, p, r.ownerScheme(), r.OperationIntentAuth)
	require.NoError(t, err)
	require.NoError(t, r.Create(context.Background(), other))
	ref, ok, err := r.loadRedactedIntent(context.Background(), vm, a.OpID, clusteredNS)
	require.NoError(t, err, "the other redacted Secret authenticates")
	require.True(t, ok)
	require.NotEmpty(t, ref.Digest)
	running = false
	reconcileN(t, r, req, 3)
	assert.Len(t, secretsByPrefix(t, r, operationIntentSecretPrefix), 1, "the full intent is kept")
	warnings := 0
	for len(rec.Events) > 0 {
		if ev := <-rec.Events; strings.Contains(ev, eventReasonIntentRedactionBlocked) {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings)
}
