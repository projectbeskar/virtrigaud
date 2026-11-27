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

// One live Host per machine identity (development builds only).
//
// spec.identity names one machine. The manager enrols no Host whose
// identity (compared without regard to case) another live Host of a
// per-host agent Provider also carries, unless the Host already holds a
// current enrolment: the first holder wins and is never blocked by a later
// claimant. The holder shows the duplicate in its AgentIdentityDuplicated
// condition; a claimant is refused with a generic reason, so a Host's author
// does not learn which identities are in use. Neither names the other Host
// nor echoes the identity. Admission does not check this yet.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// reasonEnrolRefused is the generic AgentEnrolled condition reason and
// request denial reason of a Host that cannot be enrolled now because its
// identity is claimed elsewhere; it says no more than that.
const reasonEnrolRefused = "EnrolmentRefused"

// HostConditionAgentIdentityDuplicated is True on an enrolled Host while
// another live Host of a per-host agent Provider claims its identity (that
// Host is not enrolled). Development builds only.
const HostConditionAgentIdentityDuplicated = "AgentIdentityDuplicated"

// Reasons of the AgentIdentityDuplicated condition.
const (
	reasonIdentityDuplicated = "DuplicateIdentityClaimed"
	reasonIdentityUnique     = "Unique"
)

// hostIdentityIndex indexes Hosts by their normalised spec.identity.
const hostIdentityIndex = "spec.identity.normalised"

// hostIdentityIndexRegistered records the field indexers the identity index
// is registered with.
var hostIdentityIndexRegistered sync.Map

// normaliseIdentity is the comparison form of a Host identity.
func normaliseIdentity(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// indexHostIdentity registers the identity index once per manager.
func indexHostIdentity(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := hostIdentityIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	if err := indexer.IndexField(context.Background(), &infravirtrigaudiov1beta1.Host{}, hostIdentityIndex,
		func(o client.Object) []string {
			h, ok := o.(*infravirtrigaudiov1beta1.Host)
			if !ok || h.Spec.Identity == "" {
				return nil
			}
			return []string{normaliseIdentity(h.Spec.Identity)}
		}); err != nil {
		hostIdentityIndexRegistered.Delete(indexer)
		return fmt.Errorf("index Host by %s: %w", hostIdentityIndex, err)
	}
	return nil
}

// hostsWithIdentity returns the Hosts other than host (by UID), in every
// namespace, that carry host's identity (normalised), are neither revoked
// nor deleting, and belong to a Provider that runs per-host agents. With
// cached (a reader with the identity index) the candidates come from the
// index and each is confirmed with a live read; with cached nil every Host
// is listed live. Provider objects are read live.
func hostsWithIdentity(ctx context.Context, live, cached client.Reader, host *infravirtrigaudiov1beta1.Host) ([]*infravirtrigaudiov1beta1.Host, error) {
	want := normaliseIdentity(host.Spec.Identity)
	if want == "" {
		return nil, nil
	}
	var list infravirtrigaudiov1beta1.HostList
	var err error
	if cached != nil {
		err = cached.List(ctx, &list, client.MatchingFields{hostIdentityIndex: want})
	} else {
		err = live.List(ctx, &list)
	}
	if err != nil {
		return nil, fmt.Errorf("list Hosts to check that the identity of Host %s/%s is unique: %w", host.Namespace, host.Name, err)
	}
	var out []*infravirtrigaudiov1beta1.Host
	for i := range list.Items {
		o := &list.Items[i]
		if o.UID == host.UID || normaliseIdentity(o.Spec.Identity) != want {
			continue
		}
		if cached != nil {
			fresh := &infravirtrigaudiov1beta1.Host{}
			if err := live.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.Name}, fresh); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("confirm Host %s/%s: %w", o.Namespace, o.Name, err)
			}
			if fresh.UID != o.UID || normaliseIdentity(fresh.Spec.Identity) != want {
				continue
			}
			o = fresh
		}
		if o.Spec.Revoked || !o.DeletionTimestamp.IsZero() {
			continue
		}
		p := &infravirtrigaudiov1beta1.Provider{}
		if err := live.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.Spec.ProviderRef.Name}, p); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("Provider of Host %s/%s: %w", o.Namespace, o.Name, err)
		}
		if !providerRunsHostAgents(p) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}
