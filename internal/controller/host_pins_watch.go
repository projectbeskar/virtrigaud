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
	"encoding/json"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// pinsTrustChanged passes a pins Secret update only when something other
// than the store positions changed. The heartbeat records the host's verified
// store position in the store state key on every answer; such a write
// changes nothing a watcher of the pins decides on (pins, expiry,
// revocation, renewal, re-key), so it does not enqueue. A store state write
// that changes the regression hold, or one that cannot be decoded, still
// passes. Creates, deletes and generic events always pass.
func pinsTrustChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldS, ok1 := e.ObjectOld.(*corev1.Secret)
			newS, ok2 := e.ObjectNew.(*corev1.Secret)
			if !ok1 || !ok2 {
				return true
			}
			return !onlyStorePositionsChanged(oldS, newS)
		},
	}
}

// pinsAdvanceHost passes a pins Secret update only when it is a step the
// Host controller's poll must follow at once:
//
//   - it lifts an expired agent certificate (a re-key): the current
//     certificate had expired and a valid one is pinned now, so the Host
//     does not keep CertificateExpired until the next heartbeat;
//   - it pins the host's first certificate: the Provider fronts the host
//     from now on, and the Host's first poll is not left to its backoff;
//   - it is a step of a certificate renewal: a renewal recorded, its request
//     created, approved or issued, its new key pinned, the renewal ended, or
//     the key pins changed while one is in progress. The poll carries the
//     renewal (the Provider stages the issued certificate on its next call
//     to the host, and the manager pins on the host's next answer), and the
//     agent draws a new key when its request is not answered within half
//     the lifetime left, so a step left to the next heartbeat could make a
//     short-lived certificate miss its renewal.
//
// Every other change waits for the Host's own schedule: the writes of an
// enrolment before its pin or of a revocation would only poll a host its
// Provider does not front, and the heartbeat's store position writes would
// add a poll per heartbeat. Creates, deletes and generic events do not pass
// (a Host is reconciled at start-up through its own watch). now nil:
// time.Now.
func pinsAdvanceHost(now func() time.Time) predicate.Funcs {
	if now == nil {
		now = time.Now
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldS, ok1 := e.ObjectOld.(*corev1.Secret)
			newS, ok2 := e.ObjectNew.(*corev1.Secret)
			if !ok1 || !ok2 {
				return false
			}
			before, okOld := decodeEnrolmentState(oldS)
			after, okNew := decodeEnrolmentState(newS)
			if !okOld || !okNew {
				return false
			}
			return liftsExpiry(before, after, now()) || firstPin(before, after) || renewalStep(before, after)
		},
	}
}

// decodeEnrolmentState decodes the enrolment state a pins Secret records;
// false when it cannot be decoded.
func decodeEnrolmentState(s *corev1.Secret) (hostEnrolmentState, bool) {
	var st hostEnrolmentState
	if data := s.Data[hostEnrolmentStateKey]; len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			return hostEnrolmentState{}, false
		}
	}
	return st, true
}

// liftsExpiry reports whether an expired current certificate was replaced by
// a valid one at at.
func liftsExpiry(before, after hostEnrolmentState, at time.Time) bool {
	return before.Current != nil && after.Current != nil &&
		!at.Before(before.Current.NotAfter.Time) && at.Before(after.Current.NotAfter.Time)
}

// firstPin reports whether the host's first certificate was pinned.
func firstPin(before, after hostEnrolmentState) bool {
	return before.Current == nil && after.Current != nil
}

// renewalStep reports whether a certificate renewal moved a step.
func renewalStep(before, after hostEnrolmentState) bool {
	o, n := before.Renewal, after.Renewal
	if (o == nil) != (n == nil) {
		return true
	}
	if o == nil {
		return false
	}
	if o.KeyPin != n.KeyPin || o.CertificateRequest != n.CertificateRequest ||
		o.CertificateRequestUID != n.CertificateRequestUID || o.Approved != n.Approved ||
		o.CertificateSHA256 != n.CertificateSHA256 || !bytes.Equal(o.Certificate, n.Certificate) ||
		(o.PinnedAt == nil) != (n.PinnedAt == nil) {
		return true
	}
	var op, np []string
	if before.Current != nil {
		op = before.Current.KeyPins
	}
	if after.Current != nil {
		np = after.Current.KeyPins
	}
	return !slices.Equal(op, np)
}

// onlyStorePositionsChanged reports whether two versions of a pins Secret
// differ at most in the store positions of the store state key.
func onlyStorePositionsChanged(oldS, newS *corev1.Secret) bool {
	if oldS.Type != newS.Type ||
		!maps.Equal(oldS.Labels, newS.Labels) ||
		!maps.Equal(oldS.Annotations, newS.Annotations) ||
		!slices.Equal(oldS.Finalizers, newS.Finalizers) ||
		!equality.Semantic.DeepEqual(oldS.OwnerReferences, newS.OwnerReferences) ||
		!equality.Semantic.DeepEqual(oldS.DeletionTimestamp, newS.DeletionTimestamp) ||
		!maps.Equal(oldS.StringData, newS.StringData) {
		return false
	}
	without := func(d map[string][]byte) map[string][]byte {
		c := maps.Clone(d)
		delete(c, hostStoreStateKey)
		return c
	}
	if !maps.EqualFunc(without(oldS.Data), without(newS.Data), bytes.Equal) {
		return false
	}
	before, after := oldS.Data[hostStoreStateKey], newS.Data[hostStoreStateKey]
	if bytes.Equal(before, after) {
		return true
	}
	o, err1 := decodeStoreState(before)
	n, err2 := decodeStoreState(after)
	if err1 != nil || err2 != nil {
		return false
	}
	// A change of the members a newer manager wrote (a downgrade floor,
	// say) is not a position change.
	return o.RegressedReason == n.RegressedReason && bytes.Equal(o.RegressedEnrolmentID, n.RegressedEnrolmentID) &&
		o.unknown.equal(n.unknown) && maps.EqualFunc(o.recordUnknown, n.recordUnknown, unknownMembers.equal)
}
