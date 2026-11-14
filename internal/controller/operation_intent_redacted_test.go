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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	_, r, _, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	p, found, err := r.loadIntent(context.Background(), vm, a.OpID, clusteredNS)
	require.NoError(t, err)
	require.True(t, found)
	fullRef := refOf(p)
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
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
	stub, r, req, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
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
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
	assert.Empty(t, secretsByPrefix(t, r, operationIntentSecretPrefix))
	assert.Len(t, secretsByPrefix(t, r, redactedIntentSecretPrefix), 1)

	// A crash between writing the redacted Secret and deleting the full one.
	full.ResourceVersion, full.UID = "", ""
	require.NoError(t, r.Create(context.Background(), full))
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
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
	_, r, _, s := preparedWithIntentStub(t, stub)
	vm := idVM(t, r)
	a := findAttempt(vm, contracts.OperationKindCreate)
	r.Client = &conflictingDeleteClient{Client: r.Client, conflicts: 2}
	require.NoError(t, r.redactIntent(context.Background(), vm, a.OpID, s.Namespace))
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
