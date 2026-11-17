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

// pinsLiftExpiry passes a pins Secret update only when it lifts an expired
// agent certificate: the current enrolment's certificate had expired before
// the update and a current certificate that is still valid is pinned after
// it (a re-key). Only then must the Host be re-evaluated at once, so it does
// not keep CertificateExpired until the next heartbeat. Every other change
// waits for the Host's own schedule: an enrolment pins a host its Provider
// does not front yet, and a revocation or a renewal changes nothing the
// heartbeat would poll sooner for, so polling on them would only log
// GetHostInfo failures. Creates, deletes and generic events do not pass (a
// Host is reconciled at start-up through its own watch). now nil: time.Now.
func pinsLiftExpiry(now func() time.Time) predicate.Funcs {
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
			at := now()
			before, okOld := currentNotAfter(oldS)
			after, okNew := currentNotAfter(newS)
			return okOld && okNew && !at.Before(before) && at.Before(after)
		},
	}
}

// currentNotAfter returns the expiry of the current enrolment's certificate
// a pins Secret records; false when it records none or cannot be decoded.
func currentNotAfter(s *corev1.Secret) (time.Time, bool) {
	var st hostEnrolmentState
	if err := json.Unmarshal(s.Data[hostEnrolmentStateKey], &st); err != nil || st.Current == nil {
		return time.Time{}, false
	}
	return st.Current.NotAfter.Time, true
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
	return o.RegressedReason == n.RegressedReason && bytes.Equal(o.RegressedEnrolmentID, n.RegressedEnrolmentID)
}
