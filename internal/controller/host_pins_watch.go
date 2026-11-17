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
	"maps"
	"slices"

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
