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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The intent store of operation identity: one Secret per non-terminal
// operation attempt, in the namespace of the VM's Provider, holding the exact
// request bytes the attempt's grants cover and its digest salt. It is written
// before the attempt's marker leaves Prepared and deleted once the marker is
// Terminal, so a re-send after a manager restart sends the same bytes (a host
// refuses a known op_id with other bytes). The bytes can carry secrets
// (cloud-init user data): they are never logged, put in an event, a condition
// or an error, and never stored in the VM's status.
//
// The manager deletes only Secrets it can tell are its own intent Secrets:
// type operationIntentSecretType, label operationIntentLabel, a name with
// operationIntentSecretPrefix, and the UID it read (a delete precondition).
// RBAC cannot scope the delete verb to them (no label or name-prefix scoping),
// which is why the manager's delete on Secrets is a separate, delete-only
// rule (docs/operation-grants-manager.md).
const (
	// operationIntentSecretPrefix starts the name of an intent Secret:
	// vrtg-op-<op_id in hex>.
	operationIntentSecretPrefix = "vrtg-op-"
	// operationIntentSecretType is the type of every intent Secret.
	operationIntentSecretType corev1.SecretType = "virtrigaud.io/operation-intent" // #nosec G101 -- a Secret type name, not a credential
	// operationIntentLabel marks an intent Secret (value "true").
	operationIntentLabel = "virtrigaud.io/operation-intent"
	// operationIntentVMUIDLabel holds the UID of the VM the attempt is for.
	operationIntentVMUIDLabel = "virtrigaud.io/vm-uid"
	// operationIntentKindLabel holds the attempt's operation kind.
	operationIntentKindLabel = "virtrigaud.io/operation-kind"
	// operationIntentVMNamespaceAnnotation and operationIntentVMNameAnnotation
	// name the VM the attempt is for (the sweeper reads the VM by them).
	operationIntentVMNamespaceAnnotation = "virtrigaud.io/vm-namespace"
	operationIntentVMNameAnnotation      = "virtrigaud.io/vm-name"

	// Data keys of an intent Secret.
	intentKeyRequest  = "request"
	intentKeySalt     = "salt"
	intentKeyIdentity = "identity"
	// intentKeyTag is the intent's authentication tag
	// (operation.IntentAuthenticator).
	intentKeyTag = "tag"

	// maxIntentRequestBytes caps the request bytes of one attempt well below
	// the 1 MiB Secret limit; a larger request is held
	// (OperationRequestTooLarge), never retried.
	maxIntentRequestBytes = 768 << 10

	// managedByLabel and managedByValue mark the intent Secrets as the
	// operator's.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "virtrigaud"

	// operationIntentSweepGrace is how old an intent Secret must be before
	// the sweeper may remove it: a Secret is written just before its marker,
	// and must never be swept in between.
	operationIntentSweepGrace = 10 * time.Minute
	// operationIntentLabelTrue is the value of operationIntentLabel.
	operationIntentLabelTrue = "true"
)

// errIntentNotOurs is returned for a Secret at an intent Secret's name that is
// not an intent Secret of the expected VM: it is never used or deleted.
var errIntentNotOurs = errors.New("the Secret at the intent Secret's name is not this VM's operation-intent Secret")

// errIntentTampered is returned for an intent Secret whose content does not
// authenticate, cannot be decoded, or does not match the attempt it is read
// for. Nothing is signed or sent from it, and it is kept as evidence.
var errIntentTampered = errors.New("the operation-intent Secret does not authenticate or does not match its attempt")

// errIntentTooLarge is returned for a request larger than
// maxIntentRequestBytes.
var errIntentTooLarge = errors.New("the operation's request is too large to store")

// intentIdentity is the JSON form of an attempt's identity in its intent
// Secret.
type intentIdentity struct {
	Kind     contracts.OperationKind            `json:"kind"`
	Identity contracts.OperationAttemptIdentity `json:"identity"`
}

// intentSecretName is the name of the intent Secret of op_id opID (hex).
func intentSecretName(opID string) string {
	return operationIntentSecretPrefix + opID
}

// intentReader is the reader intent Secrets are read through: the API server
// (APIReader) when the reconciler has one, so no Secret informer is needed
// and a Secret written a moment ago is seen.
func (r *VirtualMachineReconciler) intentReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// intentMAC is the authentication input of an intent Secret: its namespace
// and name, the VM it is for (namespace, name, UID label), and the exact
// identity, salt and request bytes it stores (operation.IntentMessage).
func intentMAC(s *corev1.Secret) []byte {
	return operation.IntentMessage(
		[]byte(s.Namespace), []byte(s.Name),
		[]byte(s.Annotations[operationIntentVMNamespaceAnnotation]), []byte(s.Annotations[operationIntentVMNameAnnotation]),
		[]byte(s.Labels[operationIntentVMUIDLabel]), []byte(s.Labels[operationIntentKindLabel]),
		s.Data[intentKeyIdentity], s.Data[intentKeySalt], s.Data[intentKeyRequest],
	)
}

// buildIntentSecret is the authenticated, immutable intent Secret of p for
// vm, in the namespace of provider and owned by it (garbage-collected with
// it).
func buildIntentSecret(provider *infravirtrigaudiov1beta1.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.PreparedOperation, scheme *runtime.Scheme, auth operation.IntentAuthenticator) (*corev1.Secret, error) {
	if auth == nil {
		return nil, operation.ErrNoIntentAuthenticator
	}
	if len(p.RequestBytes) > maxIntentRequestBytes {
		return nil, errIntentTooLarge
	}
	ident, err := json.Marshal(intentIdentity{Kind: p.Kind, Identity: p.Identity})
	if err != nil {
		return nil, fmt.Errorf("encode the attempt identity: %w", err)
	}
	immutable := true
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      intentSecretName(hex.EncodeToString(p.Identity.OpID)),
			Namespace: provider.Namespace,
			Labels: map[string]string{
				operationIntentLabel:      operationIntentLabelTrue,
				operationIntentVMUIDLabel: string(vm.UID),
				operationIntentKindLabel:  string(p.Kind),
				managedByLabel:            managedByValue,
			},
			Annotations: map[string]string{
				operationIntentVMNamespaceAnnotation: vm.Namespace,
				operationIntentVMNameAnnotation:      vm.Name,
			},
		},
		Type:      operationIntentSecretType,
		Immutable: &immutable,
		Data: map[string][]byte{
			intentKeyRequest:  append([]byte(nil), p.RequestBytes...),
			intentKeySalt:     append([]byte(nil), p.DigestSalt...),
			intentKeyIdentity: ident,
		},
	}
	s.Data[intentKeyTag] = auth.SealIntent(intentMAC(s))
	if err := controllerutil.SetControllerReference(provider, s, scheme); err != nil {
		return nil, fmt.Errorf("set the Provider as the intent Secret's owner: %w", err)
	}
	return s, nil
}

// storeIntent writes the intent Secret of p. A Secret already at its name
// that authenticates and holds exactly p (a retried write) is accepted;
// anything else is an error, and nothing is sent.
func (r *VirtualMachineReconciler) storeIntent(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, p contracts.PreparedOperation) error {
	s, err := buildIntentSecret(provider, vm, p, r.ownerScheme(), r.OperationIntentAuth)
	if err != nil {
		return err
	}
	err = r.Create(ctx, s)
	if err == nil {
		return nil
	}
	if apierrors.IsRequestEntityTooLargeError(err) || (apierrors.IsInvalid(err) && len(p.RequestBytes) > maxIntentRequestBytes/2) {
		return fmt.Errorf("create the operation-intent Secret: %w", errIntentTooLarge)
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the operation-intent Secret: %w", err)
	}
	existing, found, rerr := r.loadIntent(ctx, vm, hex.EncodeToString(p.Identity.OpID), provider.Namespace)
	if rerr != nil {
		return rerr
	}
	if !found || !bytes.Equal(existing.RequestBytes, p.RequestBytes) || !bytes.Equal(existing.DigestSalt, p.DigestSalt) {
		return fmt.Errorf("an operation-intent Secret with other content already exists: %w", errIntentTampered)
	}
	return nil
}

// ownIntent reports whether s is the intent Secret of op_id opID for the VM
// of UID vmUID.
func ownIntent(s *corev1.Secret, opID string, vmUID types.UID) bool {
	return s.Type == operationIntentSecretType && s.Name == intentSecretName(opID) &&
		s.Labels[operationIntentLabel] == operationIntentLabelTrue && s.Labels[operationIntentVMUIDLabel] == string(vmUID)
}

// loadIntent reads the intent Secret of op_id opID (hex) for vm from
// namespace ns, authenticates it (OperationIntentAuth) and decodes the
// prepared operation it holds. found is false when there is no Secret at
// that name. A Secret that is not vm's intent Secret, does not authenticate,
// or names another op_id, VM, VM namespace or name returns errIntentTampered
// (errIntentNotOurs for a foreign Secret); nothing from it is used.
func (r *VirtualMachineReconciler) loadIntent(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, opID, ns string) (
	p contracts.PreparedOperation, found bool, err error) {
	if r.OperationIntentAuth == nil {
		return p, false, operation.ErrNoIntentAuthenticator
	}
	s := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return p, false, nil
		}
		return p, false, fmt.Errorf("read the operation-intent Secret: %w", err)
	}
	if !ownIntent(s, opID, vm.UID) {
		return p, true, fmt.Errorf("%w: %w", errIntentTampered, errIntentNotOurs)
	}
	if !r.OperationIntentAuth.OpenIntent(intentMAC(s), s.Data[intentKeyTag]) {
		return p, true, fmt.Errorf("authenticate: %w", errIntentTampered)
	}
	if s.Annotations[operationIntentVMNamespaceAnnotation] != vm.Namespace || s.Annotations[operationIntentVMNameAnnotation] != vm.Name {
		return p, true, fmt.Errorf("another VM: %w", errIntentTampered)
	}
	var ident intentIdentity
	if err := json.Unmarshal(s.Data[intentKeyIdentity], &ident); err != nil {
		return p, true, fmt.Errorf("decode the identity: %w", errIntentTampered)
	}
	if hex.EncodeToString(ident.Identity.OpID) != opID || string(ident.Kind) != s.Labels[operationIntentKindLabel] {
		return p, true, fmt.Errorf("another attempt: %w", errIntentTampered)
	}
	p = contracts.PreparedOperation{
		Kind:         ident.Kind,
		Identity:     ident.Identity,
		RequestBytes: s.Data[intentKeyRequest],
		DigestSalt:   s.Data[intentKeySalt],
	}
	return p, true, nil
}

// deleteIntent deletes the intent Secret of op_id opID for the VM of UID
// vmUID, if it is there and is that VM's; with the UID read as a
// precondition, so a Secret re-created at the name in between is not
// deleted.
func (r *VirtualMachineReconciler) deleteIntent(ctx context.Context, ns, opID string, vmUID types.UID) error {
	s := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read the operation-intent Secret: %w", err)
	}
	if !ownIntent(s, opID, vmUID) {
		return errIntentNotOurs
	}
	return deleteIntentSecret(ctx, r.Client, s)
}

// deleteIntentSecret deletes s with its UID as a precondition; a Secret
// already gone is not an error.
func deleteIntentSecret(ctx context.Context, c client.Client, s *corev1.Secret) error {
	uid := s.UID
	err := c.Delete(ctx, s, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the operation-intent Secret: %w", err)
	}
	return nil
}

// ownerScheme is the scheme owner references are built with: the
// reconciler's, or its client's.
func (r *VirtualMachineReconciler) ownerScheme() *runtime.Scheme {
	if r.Scheme != nil {
		return r.Scheme
	}
	return r.Client.Scheme()
}

// sweepOperationIntents removes the intent Secrets of provider that no
// attempt needs any more: Secrets in its namespace, of the intent type and
// label, owned by provider and older than operationIntentSweepGrace, whose
// VM is gone, or whose VM has no non-terminal attempt marker naming the
// Secret's op_id and no Delete recorded Succeeded awaiting its finalizer's
// release under it. Such a Secret is left behind by a crash between its
// write and its marker's, by a marker write that failed ambiguously, or by a
// failed delete after the marker became Terminal. The VM is found by the
// Secret's VM-UID label among the VirtualMachines c lists, and read again
// through reader (the API server); a Secret whose VM annotations disagree
// with the VM its UID label names is skipped and logged, never deleted, and
// returned in skipped. It returns how many Secrets it deleted.
func sweepOperationIntents(ctx context.Context, c client.Client, reader client.Reader,
	provider *infravirtrigaudiov1beta1.Provider, now time.Time) (deleted int, skipped []string, err error) {
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(provider.Namespace),
		client.MatchingLabels{operationIntentLabel: operationIntentLabelTrue}); err != nil {
		return 0, nil, fmt.Errorf("list the operation-intent Secrets of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	var vms *infravirtrigaudiov1beta1.VirtualMachineList
	for i := range secrets.Items {
		s := &secrets.Items[i]
		opID, ok := strings.CutPrefix(s.Name, operationIntentSecretPrefix)
		if !ok || s.Type != operationIntentSecretType || !metav1.IsControlledBy(s, provider) ||
			now.Sub(s.CreationTimestamp.Time) < operationIntentSweepGrace {
			continue
		}
		if vms == nil {
			vms = &infravirtrigaudiov1beta1.VirtualMachineList{}
			if err := c.List(ctx, vms); err != nil {
				return deleted, skipped, fmt.Errorf("list VirtualMachines for the operation-intent sweep: %w", err)
			}
		}
		needed, mismatch, err := intentNeeded(ctx, reader, vms, s, opID)
		if err != nil {
			return deleted, skipped, err
		}
		if mismatch {
			skipped = append(skipped, s.Name)
		}
		if needed {
			continue
		}
		if err := deleteIntentSecret(ctx, c, s); err != nil {
			return deleted, skipped, err
		}
		log.FromContext(ctx).Info("Removed an operation-intent Secret no attempt needs any more", "secret", s.Name)
		deleted++
	}
	return deleted, skipped, nil
}

// eventReasonOperationIntentKept is the Provider Warning event reason of an
// intent Secret the sweeper keeps because its VM annotations disagree with
// its VM-UID label.
const eventReasonOperationIntentKept = "OperationIntentKept"

// errIntentVMMismatch is intentNeeded's answer for a Secret whose VM
// annotations disagree with the VM its UID label names: it is kept.
var errIntentVMMismatch = errors.New("the operation-intent Secret's VM annotations disagree with its VM-UID label")

// intentNeeded reports whether the VM the intent Secret s names (by its
// VM-UID label) still needs it for op_id opID. A Secret whose annotations
// disagree with that VM is reported needed (kept), with mismatch, and logged.
func intentNeeded(ctx context.Context, reader client.Reader, vms *infravirtrigaudiov1beta1.VirtualMachineList,
	s *corev1.Secret, opID string) (needed, mismatch bool, err error) {
	uid := s.Labels[operationIntentVMUIDLabel]
	key := types.NamespacedName{Namespace: s.Annotations[operationIntentVMNamespaceAnnotation], Name: s.Annotations[operationIntentVMNameAnnotation]}
	for i := range vms.Items {
		if string(vms.Items[i].UID) == uid && uid != "" && client.ObjectKeyFromObject(&vms.Items[i]) != key {
			log.FromContext(ctx).Info("Not sweeping an operation-intent Secret whose VM annotations disagree with its VM-UID label",
				"secret", s.Name, "error", errIntentVMMismatch.Error())
			return true, true, nil
		}
	}
	if key.Namespace == "" || key.Name == "" {
		return false, false, nil
	}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := reader.Get(ctx, key, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read VirtualMachine %s for operation-intent Secret %s: %w", key, s.Name, err)
	}
	if string(vm.UID) != uid {
		return false, false, nil
	}
	for _, a := range vm.Status.OperationAttempts {
		if a.OpID != opID {
			continue
		}
		if a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal || deleteAwaitingRelease(vm, &a) {
			return true, false, nil
		}
	}
	return false, false, nil
}

// sweepOperationIntents runs sweepOperationIntents for provider, logging
// (never failing the reconcile on) an error.
func (r *ProviderReconciler) sweepOperationIntents(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	_, skipped, err := sweepOperationIntents(ctx, r.Client, reader, provider, time.Now())
	if err != nil {
		log.FromContext(ctx).Info("Could not sweep the Provider's operation-intent Secrets; retried on the next reconcile",
			"error", err.Error())
	}
	for _, name := range skipped {
		key := string(provider.UID) + "/" + name
		if _, seen := r.intentSkipWarned.LoadOrStore(key, struct{}{}); seen || r.Recorder == nil {
			continue
		}
		r.Recorder.Event(provider, corev1.EventTypeWarning, eventReasonOperationIntentKept, fmt.Sprintf(
			"Operation-intent Secret %s names a VirtualMachine that disagrees with its VM-UID label; it is kept, not swept. "+
				"Investigate who wrote it, then delete it by hand (docs/operation-grants-manager.md)", name))
	}
}
