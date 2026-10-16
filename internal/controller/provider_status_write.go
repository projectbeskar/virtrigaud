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
	stderrors "errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Provider status writes.
//
// Two controllers write a Provider's status: the Provider controller owns
// every field except status.adoption, which the VMAdoption controller owns.
// Neither may write the other's field from a copy it read earlier: the
// Provider controller writes often (every health check), so a whole-status
// copy from either side drops what the other just wrote.
//
//   - writeProviderStatus (Provider controller) writes under the
//     resourceVersion it read. On a conflict it re-reads the Provider and, if
//     the fields it owns are still as it read them (the conflict came from
//     status.adoption or metadata), writes its fields onto the live object,
//     keeping the live status.adoption. If its own fields changed meanwhile,
//     what it computed may be stale: errProviderStatusMovedOn, and the
//     reconcile runs again.
//   - writeAdoptionStatus (VMAdoption controller) applies a change to
//     status.adoption alone on the Provider re-read from the API server,
//     retrying conflicts, so it never writes the Provider controller's
//     fields and is not lost to its writes.

// errProviderStatusMovedOn means the Provider controller's own status fields
// changed since the reconcile read the Provider, so the status it computed is
// not written over them.
var errProviderStatusMovedOn = stderrors.New("the Provider's status changed since this reconcile read it")

// providerStatusConflictRequeue is when a Provider reconcile whose status
// was not written, because its own fields had moved past what it read
// (errProviderStatusMovedOn), runs again. The newer write's own watch event
// usually re-drives it sooner; it is never an immediate requeue.
const providerStatusConflictRequeue = 2 * time.Second

// isProviderStatusContention reports whether err is a Provider controller
// status write refused because its own fields changed since the read
// (errProviderStatusMovedOn): a requeue, not an error.
func isProviderStatusContention(err error) bool {
	return stderrors.Is(err, errProviderStatusMovedOn)
}

// errProviderRecreated means the Provider was deleted and re-created (another
// UID) since it was read: nothing decided for the old object is written.
var errProviderRecreated = stderrors.New("the Provider was re-created since it was read")

// providerControllerFieldsEqual reports whether a and b agree on every status
// field the Provider controller owns (all but status.adoption).
func providerControllerFieldsEqual(a, b *infravirtrigaudiov1beta1.ProviderStatus) bool {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	ac.Adoption, bc.Adoption = nil, nil
	return equality.Semantic.DeepEqual(ac, bc)
}

// writeProviderStatus writes provider.Status through the status subresource.
// base is the status as this reconcile read it (before it computed
// provider.Status). The first attempt writes under provider's
// resourceVersion. On a conflict the Provider is re-read from the API server:
// the same Provider (UID) whose Provider-controller fields still equal base
// gets those fields from provider.Status and keeps its live status.adoption
// (owned by the VMAdoption controller); otherwise nothing is written
// (errProviderStatusMovedOn / errProviderRecreated). On success provider holds
// the object as written.
func (r *ProviderReconciler) writeProviderStatus(ctx context.Context, base *infravirtrigaudiov1beta1.ProviderStatus,
	provider *infravirtrigaudiov1beta1.Provider) error {
	provider.Status.ObservedGeneration = provider.Generation
	desired := provider.Status.DeepCopy()
	written := provider
	attempt := 0
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		attempt++
		if attempt > 1 {
			live := &infravirtrigaudiov1beta1.Provider{}
			if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(provider), live); err != nil {
				return fmt.Errorf("re-read Provider %s/%s after a status conflict: %w", provider.Namespace, provider.Name, err)
			}
			if live.UID != provider.UID {
				return fmt.Errorf("Provider %s/%s: %w", provider.Namespace, provider.Name, errProviderRecreated)
			}
			if !providerControllerFieldsEqual(&live.Status, base) {
				return fmt.Errorf("Provider %s/%s: %w", provider.Namespace, provider.Name, errProviderStatusMovedOn)
			}
			adoption := live.Status.Adoption
			desired.DeepCopyInto(&live.Status)
			live.Status.Adoption = adoption
			written = live
			log.FromContext(ctx).V(1).Info("Provider status conflict on another controller's field; writing this controller's fields on the live object",
				"attempt", attempt)
		}
		return r.Status().Update(ctx, written)
	})
	if err != nil {
		return fmt.Errorf("update status of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	if written != provider {
		written.DeepCopyInto(provider)
	}
	return nil
}

// writeAdoptionStatus applies mutate to the status of the Provider re-read
// from the API server and writes the resulting status.adoption — and nothing
// else — retrying conflicts. mutate receives a copy of the live status; any
// change it makes outside status.adoption is discarded. A Provider re-created
// since it was read (another UID) is not written (errProviderRecreated). On
// success provider holds the live object as written.
func (r *VMAdoptionReconciler) writeAdoptionStatus(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	mutate func(*infravirtrigaudiov1beta1.ProviderStatus)) error {
	var written *infravirtrigaudiov1beta1.Provider
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &infravirtrigaudiov1beta1.Provider{}
		if err := r.freshReader().Get(ctx, client.ObjectKeyFromObject(provider), live); err != nil {
			return fmt.Errorf("re-read Provider %s/%s to write its adoption status: %w", provider.Namespace, provider.Name, err)
		}
		if live.UID != provider.UID {
			return fmt.Errorf("Provider %s/%s: %w", provider.Namespace, provider.Name, errProviderRecreated)
		}
		want := live.Status.DeepCopy()
		mutate(want)
		if equality.Semantic.DeepEqual(live.Status.Adoption, want.Adoption) {
			written = live
			return nil
		}
		live.Status.Adoption = want.Adoption
		if err := r.Status().Update(ctx, live); err != nil {
			return err
		}
		written = live
		return nil
	})
	if err != nil {
		return fmt.Errorf("update adoption status of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	written.DeepCopyInto(provider)
	return nil
}

// adoptionProviderNotReadyMessage is status.adoption.message while the
// Provider is not ready.
const adoptionProviderNotReadyMessage = "Provider not ready"

// writeAdoptionMessage sets status.adoption.message alone
// (writeAdoptionStatus): the last discovery's time and counts stay as stored.
func (r *VMAdoptionReconciler) writeAdoptionMessage(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, msg string) error {
	return r.writeAdoptionStatus(ctx, provider, func(st *infravirtrigaudiov1beta1.ProviderStatus) {
		ensureAdoptionStatus(st).Message = msg
	})
}

// writeAdoptionResult records a discovery's outcome — its time, the counts
// and the message from result — in status.adoption (writeAdoptionStatus).
func (r *VMAdoptionReconciler) writeAdoptionResult(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	result *infravirtrigaudiov1beta1.ProviderAdoptionStatus) error {
	result = result.DeepCopy()
	return r.writeAdoptionStatus(ctx, provider, func(st *infravirtrigaudiov1beta1.ProviderStatus) {
		a := ensureAdoptionStatus(st)
		a.LastDiscoveryTime = result.LastDiscoveryTime
		a.DiscoveredVMs = result.DiscoveredVMs
		a.AdoptedVMs = result.AdoptedVMs
		a.FailedAdoptions = result.FailedAdoptions
		a.Message = result.Message
	})
}

// ensureAdoptionStatus returns st.Adoption, initialising it when nil.
func ensureAdoptionStatus(st *infravirtrigaudiov1beta1.ProviderStatus) *infravirtrigaudiov1beta1.ProviderAdoptionStatus {
	if st.Adoption == nil {
		st.Adoption = &infravirtrigaudiov1beta1.ProviderAdoptionStatus{}
	}
	return st.Adoption
}
