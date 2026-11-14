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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Redacted intents. The latest Create of a VM that its host signed not
// performed is asked about again before the VM is released without a Delete
// (signedNotPerformed), which needs the attempt's identity and intent digest,
// never its request. So when such a Create goes Terminal its intent is
// redacted: a second, immutable Secret vrtg-opr-<op_id> holds only the
// identity, the digest salt and the intent digest (SHA-256 of salt and
// request), authenticated under its own domain (RedactedIntentMessage), and
// the full intent Secret, with the request bytes and any cloud-init user
// data, is deleted. An intent Secret is immutable, so the redaction is
// "write the redacted one, then delete the full one": a crash in between
// leaves both (the next reconcile finishes the redaction), never neither.
// A redacted intent decodes to an intentRef, which has no request bytes and
// which no send path accepts, so it can be asked about and never re-sent.

const (
	// redactedIntentSecretPrefix starts the name of a redacted intent
	// Secret: vrtg-opr-<op_id in hex>.
	redactedIntentSecretPrefix = "vrtg-opr-"
	// intentKeyDigest is the data key of a redacted intent's digest.
	intentKeyDigest = "digest"
)

// intentRef is what asking a host about an attempt needs of its intent: its
// kind, identity and intent digest. It carries no request bytes, so it
// cannot be sent.
type intentRef struct {
	// Kind is the operation.
	Kind contracts.OperationKind
	// Identity is what the attempt's grants named.
	Identity contracts.OperationAttemptIdentity
	// Digest is the intent digest of the attempt's exact request bytes.
	Digest []byte
}

// refOf is the intentRef of a full prepared operation.
func refOf(p contracts.PreparedOperation) intentRef {
	return intentRef{Kind: p.Kind, Identity: p.Identity, Digest: operation.IntentDigest(p.DigestSalt, p.RequestBytes)}
}

// redactedIntentName is the name of the redacted intent Secret of op_id opID.
func redactedIntentName(opID string) string {
	return redactedIntentSecretPrefix + opID
}

// redactedIntentMAC is the authentication input of a redacted intent Secret:
// as intentMAC, with the intent digest in place of the request bytes, under
// the redacted domain.
func redactedIntentMAC(s *corev1.Secret) []byte {
	return operation.RedactedIntentMessage(
		[]byte(s.Namespace), []byte(s.Name),
		[]byte(s.Annotations[operationIntentVMNamespaceAnnotation]), []byte(s.Annotations[operationIntentVMNameAnnotation]),
		[]byte(s.Labels[operationIntentVMUIDLabel]), []byte(s.Labels[operationIntentKindLabel]),
		s.Data[intentKeyIdentity], s.Data[intentKeySalt], s.Data[intentKeyDigest],
	)
}

// ownRedactedIntent reports whether s is the redacted intent Secret of op_id
// opID for the VM of UID vmUID.
func ownRedactedIntent(s *corev1.Secret, opID string, vmUID types.UID) bool {
	return s.Type == operationIntentSecretType && s.Name == redactedIntentName(opID) &&
		s.Labels[operationIntentLabel] == operationIntentLabelTrue && s.Labels[operationIntentVMUIDLabel] == string(vmUID)
}

// buildRedactedIntentSecret is the redacted form of full, an intent Secret
// verified in this reconcile that holds p: the same labels, annotations and
// owner, the identity and salt copied, the digest computed from its request
// bytes, and no request.
func buildRedactedIntentSecret(full *corev1.Secret, p contracts.PreparedOperation, auth operation.IntentAuthenticator) *corev1.Secret {
	immutable := true
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            redactedIntentName(hex.EncodeToString(p.Identity.OpID)),
			Namespace:       full.Namespace,
			Labels:          map[string]string{},
			Annotations:     map[string]string{},
			OwnerReferences: append([]metav1.OwnerReference(nil), full.OwnerReferences...),
		},
		Type:      operationIntentSecretType,
		Immutable: &immutable,
		Data: map[string][]byte{
			intentKeyIdentity: append([]byte(nil), full.Data[intentKeyIdentity]...),
			intentKeySalt:     append([]byte(nil), p.DigestSalt...),
			intentKeyDigest:   operation.IntentDigest(p.DigestSalt, p.RequestBytes),
		},
	}
	for _, k := range []string{operationIntentLabel, operationIntentVMUIDLabel, operationIntentKindLabel, managedByLabel} {
		s.Labels[k] = full.Labels[k]
	}
	for _, k := range []string{operationIntentVMNamespaceAnnotation, operationIntentVMNameAnnotation} {
		s.Annotations[k] = full.Annotations[k]
	}
	s.Data[intentKeyTag] = auth.SealIntent(redactedIntentMAC(s))
	return s
}

// loadRedactedIntent reads and authenticates the redacted intent Secret of
// op_id opID for vm from namespace ns. found is false when there is none. A
// Secret that is not vm's, does not authenticate, or names another op_id,
// VM or kind returns errIntentTampered.
func (r *VirtualMachineReconciler) loadRedactedIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, opID, ns string) (
	intentRef, bool, error) {
	if r.OperationIntentAuth == nil {
		return intentRef{}, false, operation.ErrNoIntentAuthenticator
	}
	s := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: redactedIntentName(opID)}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return intentRef{}, false, nil
		}
		return intentRef{}, false, fmt.Errorf("read the redacted operation-intent Secret: %w", err)
	}
	if !ownRedactedIntent(s, opID, vm.UID) {
		return intentRef{}, true, fmt.Errorf("%w: %w", errIntentTampered, errIntentNotOurs)
	}
	if _, has := s.Data[intentKeyRequest]; has {
		return intentRef{}, true, fmt.Errorf("a redacted intent with request bytes: %w", errIntentTampered)
	}
	if !r.OperationIntentAuth.OpenIntent(redactedIntentMAC(s), s.Data[intentKeyTag]) {
		return intentRef{}, true, fmt.Errorf("authenticate: %w", errIntentTampered)
	}
	if s.Annotations[operationIntentVMNamespaceAnnotation] != vm.Namespace || s.Annotations[operationIntentVMNameAnnotation] != vm.Name {
		return intentRef{}, true, fmt.Errorf("another VM: %w", errIntentTampered)
	}
	var ident intentIdentity
	if err := json.Unmarshal(s.Data[intentKeyIdentity], &ident); err != nil {
		return intentRef{}, true, fmt.Errorf("decode the identity: %w", errIntentTampered)
	}
	if hex.EncodeToString(ident.Identity.OpID) != opID || string(ident.Kind) != s.Labels[operationIntentKindLabel] ||
		len(s.Data[intentKeyDigest]) != sha256.Size {
		return intentRef{}, true, fmt.Errorf("another attempt: %w", errIntentTampered)
	}
	return intentRef{Kind: ident.Kind, Identity: ident.Identity, Digest: append([]byte(nil), s.Data[intentKeyDigest]...)}, true, nil
}

// loadPollIntent loads what asking the host about attempt opID needs: from
// its full intent when it is there, else from its redacted one.
func (r *VirtualMachineReconciler) loadPollIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, opID, ns string) (
	intentRef, bool, error) {
	p, found, err := r.loadIntent(ctx, vm, opID, ns)
	if err != nil {
		return intentRef{}, found, err
	}
	if found {
		return refOf(p), true, nil
	}
	return r.loadRedactedIntent(ctx, vm, opID, ns)
}

// errRedactionMismatch is redactIntent's answer for a redacted intent already
// at its name that does not match the full one.
var errRedactionMismatch = errors.New("a redacted operation-intent Secret with other content already exists")

// redactIntent redacts the intent of attempt opID of vm in namespace ns:
// writes its redacted form, then deletes the full Secret (with its UID and
// resourceVersion as preconditions, a conflict retried with a fresh read).
// It is idempotent: with no full Secret there is nothing to do, and a
// redacted Secret already written (by an interrupted redaction) is accepted
// when it holds the same identity, salt and digest. A full Secret that does
// not authenticate is kept as evidence and not redacted.
func (r *VirtualMachineReconciler) redactIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, opID, ns string) error {
	p, found, err := r.loadIntent(ctx, vm, opID, ns)
	if err != nil || !found {
		return err
	}
	full := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, full); err != nil {
		return client.IgnoreNotFound(err)
	}
	red := buildRedactedIntentSecret(full, p, r.OperationIntentAuth)
	if err := r.Create(ctx, red); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the redacted operation-intent Secret: %w", err)
		}
		existing, ok, lerr := r.loadRedactedIntent(ctx, vm, opID, ns)
		if lerr != nil {
			return lerr
		}
		want := refOf(p)
		if !ok || !bytes.Equal(existing.Digest, want.Digest) || !bytes.Equal(existing.Identity.OpID, want.Identity.OpID) {
			return errRedactionMismatch
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &corev1.Secret{}
		if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !ownIntent(cur, opID, vm.UID) {
			return errIntentNotOurs
		}
		uid, rv := cur.UID, cur.ResourceVersion
		if err := r.Delete(ctx, cur, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the full operation-intent Secret: %w", err)
		}
		return nil
	})
}
