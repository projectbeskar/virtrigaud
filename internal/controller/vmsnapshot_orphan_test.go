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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the re-review's M-b: a VM detached with orphan-on-delete
// keeps its snapshots (deleteOnVMDelete does not fire), and a VMSnapshot whose
// VM is gone is released with a SnapshotLeftOnHypervisor Warning, never a
// Normal "there was no provider snapshot to delete".

func TestVMSnapshotRetention_OrphanedVMKeepsItsSnapshots(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
	vm.Annotations = map[string]string{infrav1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	now := metav1.Now()
	vm.DeletionTimestamp = &now
	r, _ := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"),
		readySnapshot(time.Minute, &infrav1beta1.SnapshotRetentionPolicy{DeleteOnVMDelete: true}))
	spy := &snapshotDeleteSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }

	reconcileSnapshotKey(t, r)
	assert.False(t, snapshotGone(t, r), "the detached VM keeps running with its snapshot")
	assert.Zero(t, spy.calls.Load())
}

func TestVMSnapshot_DeleteWithTheVMGoneWarnsInsteadOfClaimingNothingToDelete(t *testing.T) {
	r, rec := newSnapPhaseReconciler(t, readySnapshot(time.Minute, nil))
	require.NoError(t, r.Delete(context.Background(), readySnapshot(time.Minute, nil)))

	reconcileSnapshotKey(t, r)
	assert.True(t, snapshotGone(t, r), "released: nothing can delete it")
	evs := strings.Join(drainRecordedEvents(rec), "\n")
	assert.Contains(t, evs, "Warning "+eventReasonSnapshotLeftOnHypervisor)
	assert.Contains(t, evs, `snapshot "snap-1"`)
	assert.NotContains(t, evs, "no provider snapshot to delete")
}
