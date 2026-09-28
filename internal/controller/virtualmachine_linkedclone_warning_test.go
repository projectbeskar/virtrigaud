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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestNoteLinkedCloneDependents pins the warning for the source of an existing
// libvirt linked clone: a non-zero count the provider reports (taken at the
// VM's last start) sets LinkedClonesDependOnDisk=True with one Warning event
// when it becomes true; zero removes it; no count leaves it as it was; Ready
// is never touched.
func TestNoteLinkedCloneDependents(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := &VirtualMachineReconciler{Recorder: rec}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "default", Generation: 3}}
	key := contracts.ProviderRawLinkedCloneDependentsKey

	r.noteLinkedCloneDependents(vm, map[string]string{})
	assert.Empty(t, vm.Status.Conditions, "no count: nothing to say")

	r.noteLinkedCloneDependents(vm, map[string]string{key: "2"})
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionLinkedClonesDependOnDisk)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, int64(3), c.ObservedGeneration)
	assert.Contains(t, c.Message, "2 other VM(s)")
	assert.Contains(t, c.Message, "powering this VM on while its linked clones are shut off corrupts them")
	assert.Contains(t, c.Message, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
	assert.Nil(t, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady), "Ready is untouched")
	require.Len(t, rec.Events, 1)
	assert.Contains(t, <-rec.Events, "Warning "+k8s.ConditionLinkedClonesDependOnDisk)

	r.noteLinkedCloneDependents(vm, map[string]string{key: "2"})
	assert.Empty(t, rec.Events, "no repeated Warning while it stays true")

	r.noteLinkedCloneDependents(vm, map[string]string{})
	assert.NotNil(t, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionLinkedClonesDependOnDisk),
		"a provider restart (no count) keeps the last warning")

	r.noteLinkedCloneDependents(vm, map[string]string{key: "0"})
	assert.Nil(t, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionLinkedClonesDependOnDisk), "the clones are gone")

	r.noteLinkedCloneDependents(vm, map[string]string{key: "garbage"})
	assert.Empty(t, vm.Status.Conditions)
}
