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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestLegacySnapshotOfVM (re-review L-3): a snapshot taken by an older manager
// is bound to the VM only when that is provable across the two clocks — the VM
// predates the VMSnapshot object (both API-server timestamps), or predates the
// manager-written status.creationTime by more than the skew margin. The band
// within the margin is ambiguous and is another VM.
func TestLegacySnapshotOfVM(t *testing.T) {
	taken := time.Now().Add(-time.Hour)
	legacy := func(objectCreated time.Time) *infrav1beta1.VMSnapshot {
		s := &infrav1beta1.VMSnapshot{}
		s.CreationTimestamp = metav1.NewTime(objectCreated)
		s.Status.CreationTime = &metav1.Time{Time: taken}
		return s
	}
	vmAt := func(created time.Time) *infrav1beta1.VirtualMachine {
		vm := &infrav1beta1.VirtualMachine{}
		vm.UID = "vm-uid"
		vm.CreationTimestamp = metav1.NewTime(created)
		return vm
	}
	for name, tc := range map[string]struct {
		snap        *infrav1beta1.VMSnapshot
		vm          *infrav1beta1.VirtualMachine
		known, same bool
	}{
		"VM well before the snapshot": {legacy(taken.Add(-time.Minute)), vmAt(taken.Add(-time.Hour)), true, true},
		"VM before the VMSnapshot object, even if the manager's clock is behind": {
			legacy(taken.Add(time.Minute)), vmAt(taken.Add(30 * time.Second)), true, true},
		"VM created in the same second as the VMSnapshot object (one manifest)": {
			legacy(taken.Add(-time.Minute).Truncate(time.Second)), vmAt(taken.Add(-time.Minute).Truncate(time.Second)), true, true},
		"VM within the skew margin (ambiguous)": {legacy(taken.Add(-time.Hour)), vmAt(taken.Add(-10 * time.Second)), true, false},
		"VM after the snapshot":                 {legacy(taken.Add(-time.Hour)), vmAt(taken.Add(time.Minute)), true, false},
		"no creation time":                      {&infrav1beta1.VMSnapshot{}, vmAt(taken), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			known, same := legacySnapshotOfVM(tc.snap, tc.vm)
			assert.Equal(t, tc.known, known)
			assert.Equal(t, tc.same, same)
			assert.Equal(t, tc.known && !tc.same, snapshotOfOtherVM(tc.snap, tc.vm))
		})
	}
}

// TestVMSnapshotBinding_AmbiguousLegacyBandIsReleasedWithAWarning: a legacy
// snapshot whose VM was created within the skew margin of the snapshot gets no
// provider call on delete and is released with a SnapshotLeftOnHypervisor
// Warning.
func TestVMSnapshotBinding_AmbiguousLegacyBandIsReleasedWithAWarning(t *testing.T) {
	snapTaken := time.Now().Add(-time.Minute) // readySnapshot(time.Minute, ...)
	r, spy, events := deletingBoundSnapshot(t, snapshotVMWithUID("vm-uid-2", snapTaken.Add(-10*time.Second)), "", "")
	reconcileSnapshotKey(t, r)
	assert.Zero(t, spy.calls.Load())
	assert.True(t, snapshotGone(t, r))
	assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning "+eventReasonSnapshotLeftOnHypervisor)
}
