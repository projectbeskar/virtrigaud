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
	operationIntentSecretType corev1.SecretType = "virtrigaud.io/operation-intent"
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
	// intentKeyAnswerID is the provider's VM id from an asynchronous Create's
	// answer, kept until the attempt is Terminal.
	intentKeyAnswerID = "answer-id"

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

// buildIntentSecret is the intent Secret of p for vm, in the namespace of
// provider and owned by it (garbage-collected with it).
func buildIntentSecret(provider *infravirtrigaudiov1beta1.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine,
	p contracts.PreparedOperation, scheme *runtime.Scheme) (*corev1.Secret, error) {
	ident, err := json.Marshal(intentIdentity{Kind: p.Kind, Identity: p.Identity})
	if err != nil {
		return nil, fmt.Errorf("encode the attempt identity: %w", err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      intentSecretName(hex.EncodeToString(p.Identity.OpID)),
			Namespace: provider.Namespace,
			Labels: map[string]string{
				operationIntentLabel:      operationIntentLabelTrue,
				operationIntentVMUIDLabel: string(vm.UID),
				operationIntentKindLabel:  string(p.Kind),
			},
			Annotations: map[string]string{
				operationIntentVMNamespaceAnnotation: vm.Namespace,
				operationIntentVMNameAnnotation:      vm.Name,
			},
		},
		Type: operationIntentSecretType,
		Data: map[string][]byte{
			intentKeyRequest:  append([]byte(nil), p.RequestBytes...),
			intentKeySalt:     append([]byte(nil), p.DigestSalt...),
			intentKeyIdentity: ident,
		},
	}
	if err := controllerutil.SetControllerReference(provider, s, scheme); err != nil {
		return nil, fmt.Errorf("set the Provider as the intent Secret's owner: %w", err)
	}
	return s, nil
}

// storeIntent writes the intent Secret of p. A Secret already at its name
// that holds exactly p (a retried write) is accepted; anything else is an
// error, and nothing is sent.
func (r *VirtualMachineReconciler) storeIntent(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, p contracts.PreparedOperation) error {
	s, err := buildIntentSecret(provider, vm, p, r.ownerScheme())
	if err != nil {
		return err
	}
	err = r.Create(ctx, s)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the operation-intent Secret: %w", err)
	}
	existing, _, found, rerr := r.loadIntent(ctx, provider.Namespace, hex.EncodeToString(p.Identity.OpID), vm.UID)
	if rerr != nil {
		return rerr
	}
	if !found || !bytes.Equal(existing.RequestBytes, p.RequestBytes) || !bytes.Equal(existing.DigestSalt, p.DigestSalt) {
		return fmt.Errorf("an operation-intent Secret with other content already exists: %w", errIntentNotOurs)
	}
	return nil
}

// ownIntent reports whether s is the intent Secret of op_id opID for the VM
// of UID vmUID.
func ownIntent(s *corev1.Secret, opID string, vmUID types.UID) bool {
	return s.Type == operationIntentSecretType && s.Name == intentSecretName(opID) &&
		s.Labels[operationIntentLabel] == operationIntentLabelTrue && s.Labels[operationIntentVMUIDLabel] == string(vmUID)
}

// loadIntent reads the intent Secret of op_id opID (hex) for the VM of UID
// vmUID from namespace ns, and decodes the prepared operation it holds and
// the answer id an asynchronous Create recorded in it. found is false when
// there is no Secret at that name.
func (r *VirtualMachineReconciler) loadIntent(ctx context.Context, ns, opID string, vmUID types.UID) (
	p contracts.PreparedOperation, answerID string, found bool, err error) {
	s := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return p, "", false, nil
		}
		return p, "", false, fmt.Errorf("read the operation-intent Secret: %w", err)
	}
	if !ownIntent(s, opID, vmUID) {
		return p, "", true, errIntentNotOurs
	}
	var ident intentIdentity
	if err := json.Unmarshal(s.Data[intentKeyIdentity], &ident); err != nil {
		return p, "", true, fmt.Errorf("decode the operation-intent Secret's identity: %w", err)
	}
	if hex.EncodeToString(ident.Identity.OpID) != opID {
		return p, "", true, errIntentNotOurs
	}
	p = contracts.PreparedOperation{
		Kind:         ident.Kind,
		Identity:     ident.Identity,
		RequestBytes: s.Data[intentKeyRequest],
		DigestSalt:   s.Data[intentKeySalt],
	}
	return p, string(s.Data[intentKeyAnswerID]), true, nil
}

// recordIntentAnswer stores the provider's VM id of an asynchronous Create in
// the attempt's intent Secret, for the record written once it is Terminal.
func (r *VirtualMachineReconciler) recordIntentAnswer(ctx context.Context, ns, opID string, vmUID types.UID, answerID string) error {
	s := &corev1.Secret{}
	if err := r.intentReader().Get(ctx, types.NamespacedName{Namespace: ns, Name: intentSecretName(opID)}, s); err != nil {
		return fmt.Errorf("read the operation-intent Secret: %w", err)
	}
	if !ownIntent(s, opID, vmUID) {
		return errIntentNotOurs
	}
	if string(s.Data[intentKeyAnswerID]) == answerID {
		return nil
	}
	s.Data[intentKeyAnswerID] = []byte(answerID)
	if err := r.Update(ctx, s); err != nil {
		return fmt.Errorf("record the Create's answer in the operation-intent Secret: %w", err)
	}
	return nil
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
// VM is gone (or replaced: another UID), or whose VM has no non-terminal
// attempt marker naming the Secret's op_id. Such a Secret is left behind by a
// crash between its write and its marker's, by a marker write that failed
// ambiguously, or by a failed delete after the marker became Terminal. The VM
// is read through reader (the API server, not a cache that may lag). It
// returns how many Secrets it deleted.
func sweepOperationIntents(ctx context.Context, c client.Client, reader client.Reader,
	provider *infravirtrigaudiov1beta1.Provider, now time.Time) (int, error) {
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(provider.Namespace),
		client.MatchingLabels{operationIntentLabel: operationIntentLabelTrue}); err != nil {
		return 0, fmt.Errorf("list the operation-intent Secrets of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	deleted := 0
	for i := range secrets.Items {
		s := &secrets.Items[i]
		opID, ok := strings.CutPrefix(s.Name, operationIntentSecretPrefix)
		if !ok || s.Type != operationIntentSecretType || !metav1.IsControlledBy(s, provider) ||
			now.Sub(s.CreationTimestamp.Time) < operationIntentSweepGrace {
			continue
		}
		needed, err := intentNeeded(ctx, reader, s, opID)
		if err != nil {
			return deleted, err
		}
		if needed {
			continue
		}
		if err := deleteIntentSecret(ctx, c, s); err != nil {
			return deleted, err
		}
		log.FromContext(ctx).Info("Removed an operation-intent Secret no attempt needs any more",
			"secret", s.Name, "vm", s.Annotations[operationIntentVMNamespaceAnnotation]+"/"+s.Annotations[operationIntentVMNameAnnotation])
		deleted++
	}
	return deleted, nil
}

// intentNeeded reports whether the VM the intent Secret s names still has a
// non-terminal attempt marker for op_id opID.
func intentNeeded(ctx context.Context, reader client.Reader, s *corev1.Secret, opID string) (bool, error) {
	key := types.NamespacedName{Namespace: s.Annotations[operationIntentVMNamespaceAnnotation], Name: s.Annotations[operationIntentVMNameAnnotation]}
	if key.Namespace == "" || key.Name == "" {
		return false, nil
	}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := reader.Get(ctx, key, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read VirtualMachine %s for operation-intent Secret %s: %w", key, s.Name, err)
	}
	if string(vm.UID) != s.Labels[operationIntentVMUIDLabel] {
		return false, nil
	}
	for _, a := range vm.Status.OperationAttempts {
		if a.OpID == opID && a.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal {
			return true, nil
		}
	}
	return false, nil
}

// sweepOperationIntents runs sweepOperationIntents for provider, logging
// (never failing the reconcile on) an error.
func (r *ProviderReconciler) sweepOperationIntents(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if _, err := sweepOperationIntents(ctx, r.Client, reader, provider, time.Now()); err != nil {
		log.FromContext(ctx).Info("Could not sweep the Provider's operation-intent Secrets; retried on the next reconcile",
			"error", err.Error())
	}
}
