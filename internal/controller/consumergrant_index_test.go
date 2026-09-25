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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// These tests pin the consumerGrantIndex the grant watches use: a grant change
// re-drives only the objects that reference the changed object (or, for a
// Namespace label change, the affected objects there), and an unrelated
// change re-drives nothing.

// grantIndexedClient is a fake client with the consumerGrantIndex registered
// for every kind that has one, as SetupWithManager registers it.
func grantIndexedClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).
		WithIndex(&infrav1beta1.VirtualMachine{}, consumerGrantIndex, vmConsumerGrantIndexValues).
		WithIndex(&infrav1beta1.VMClone{}, consumerGrantIndex, cloneConsumerGrantIndexValues).
		WithIndex(&infrav1beta1.VMMigration{}, consumerGrantIndex, migrationConsumerGrantIndexValues).
		WithIndex(&infrav1beta1.VMSnapshot{}, consumerGrantIndex, snapshotConsumerGrantIndexValues).
		Build()
}

// refusedOn returns a Ready=False/ConsumerNotAllowed condition naming the
// kind object ns/name, exactly as the controllers write it.
func refusedOn(kind, ns, name string) []metav1.Condition {
	return []metav1.Condition{{
		Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonConsumerNotAllowed,
		Message: (&ConsumerNotAllowedError{Kind: kind, Namespace: ns, Name: name}).Error(),
	}}
}

func reqNames(reqs []reconcile.Request) []string {
	out := []string{}
	for _, r := range reqs {
		out = append(out, r.String())
	}
	return out
}

func TestParseConsumerNotAllowedMessage(t *testing.T) {
	for _, kind := range []string{consumerKindProvider, consumerKindVMClass, consumerKindVMImage} {
		msg := (&ConsumerNotAllowedError{Kind: kind, Namespace: "infra", Name: "shared.v1"}).Error()
		gotKind, key, ok := parseConsumerNotAllowedMessage(msg)
		require.True(t, ok, msg)
		assert.Equal(t, kind, gotKind)
		assert.Equal(t, types.NamespacedName{Namespace: "infra", Name: "shared.v1"}, key)
	}
	for _, msg := range []string{
		"",
		"Provider infra/p is not ready",
		"Secret infra/p may not be used from this namespace: x",
		"Provider infra may not be used from this namespace: x",
		"Provider /p may not be used from this namespace: x",
		"Provider infra/ may not be used from this namespace: x",
		"Provider infra/a/b may not be used from this namespace: x",
	} {
		_, _, ok := parseConsumerNotAllowedMessage(msg)
		assert.False(t, ok, msg)
	}
}

func TestConsumerGrantIndexValues(t *testing.T) {
	vm := cgVM(cgOwnerNS, "")
	vm.Spec.ImageRef = &infrav1beta1.ObjectRef{Name: "golden", Namespace: "images"}
	assert.ElementsMatch(t, []string{
		"ref:Provider/" + cgOwnerNS + "/shared", "ref:VMImage/images/golden", "ns:" + bpNS,
	}, vmConsumerGrantIndexValues(vm), "only cross-namespace references are indexed")
	assert.Empty(t, vmConsumerGrantIndexValues(cgVM("", "")), "a same-namespace VM is not indexed")
	refused := cgVM("", "")
	refused.Status.Conditions = refusedOn(consumerKindProvider, cgOwnerNS, "shared")
	assert.Equal(t, []string{"ns:" + bpNS}, vmConsumerGrantIndexValues(refused))

	clone := xnsClone(xnsTarget)
	assert.Empty(t, cloneConsumerGrantIndexValues(clone), "an object that is not refused is not indexed")
	clone.Status.Conditions = refusedOn(consumerKindVMClass, xnsSource, "src-class")
	assert.ElementsMatch(t, []string{"ref:VMClass/" + xnsSource + "/src-class", "ns:" + xnsSource, "ns:" + xnsTarget},
		cloneConsumerGrantIndexValues(clone))

	migration, _ := xnsMigration("")
	migration.Status.Conditions = refusedOn(consumerKindProvider, cgOwnerNS, "src-prov")
	assert.ElementsMatch(t, []string{"ref:Provider/" + cgOwnerNS + "/src-prov", "ns:" + xnsSource},
		migrationConsumerGrantIndexValues(migration), "the own and target namespace are indexed once")

	snapshot := &infrav1beta1.VMSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: bpNS}}
	snapshot.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonConsumerNotAllowed, Message: "unparseable"}}
	assert.Equal(t, []string{"ns:" + bpNS}, snapshotConsumerGrantIndexValues(snapshot))

	assert.Nil(t, vmConsumerGrantIndexValues(clone), "a wrong type is not indexed")
}

func TestVMsForGrantChange(t *testing.T) {
	ctx := context.Background()
	refused := cgVM("", "")
	refused.Name = "refused"
	refused.Status.Conditions = refusedOn(consumerKindProvider, cgOwnerNS, "shared")
	cross := cgVM("", cgOwnerNS)
	cross.Name = "cross"
	crossProvider := cgVM(cgOwnerNS, "")
	crossProvider.Name = "cross-provider"
	local := cgVM("", "")
	local.Name = "local"
	elsewhere := cgVM(cgOwnerNS, "")
	elsewhere.Name = "elsewhere"
	elsewhere.Namespace = "team-b"
	r := &VirtualMachineReconciler{Client: grantIndexedClient(t, refused, cross, crossProvider, local, elsewhere)}

	ref := func(obj client.Object) string {
		v, ok := changedObjectIndexValue(obj)
		require.True(t, ok)
		return v
	}
	assert.ElementsMatch(t, []string{bpNS + "/refused", bpNS + "/cross", bpNS + "/cross-provider"},
		reqNames(r.vmsForGrantChange(ctx, consumerNamespaceIndexValue(bpNS))),
		"a Namespace label change re-drives that namespace's refused and cross-namespace VMs only")
	assert.ElementsMatch(t, []string{bpNS + "/cross-provider", "team-b/elsewhere"},
		reqNames(r.vmsForGrantChange(ctx, ref(grantedProvider(cgOwnerNS, "shared", nil)))),
		"a Provider selector change re-drives the VMs that reference it, in every namespace")
	assert.ElementsMatch(t, []string{bpNS + "/cross"},
		reqNames(r.vmsForGrantChange(ctx, ref(grantedClass(cgOwnerNS, "shared", nil)))))

	// Unrelated changes enqueue nothing.
	assert.Empty(t, r.vmsForGrantChange(ctx, ref(grantedClass("team-z", "own-class", nil))),
		"a tenant editing its own, unreferenced VMClass re-drives nothing")
	assert.Empty(t, r.vmsForGrantChange(ctx, ref(grantedProvider(bpNS, "shared", nil))),
		"a same-namespace reference is never affected by a selector")
	assert.Empty(t, r.vmsForGrantChange(ctx, consumerNamespaceIndexValue("team-z")))
}

func TestClonesMigrationsSnapshotsForGrantChange(t *testing.T) {
	ctx := context.Background()

	cloneRefused := xnsClone(xnsTarget)
	cloneRefused.Name = "refused"
	cloneRefused.Status.Conditions = refusedOn(consumerKindVMClass, xnsSource, "src-class")
	cloneRunning := xnsClone(xnsTarget)
	cloneRunning.Name = "running"
	cloneDone := xnsClone(xnsTarget)
	cloneDone.Name = "done"
	cloneDone.Status.Phase = infrav1beta1.ClonePhaseReady
	cloneDone.Status.Conditions = refusedOn(consumerKindVMClass, xnsSource, "src-class")

	migRefused, _ := xnsMigration(xnsTarget)
	migRefused.Name = "refused"
	migRefused.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migRefused.Status.Conditions = refusedOn(consumerKindProvider, xnsSource, "tgt-prov")
	migFailed, _ := xnsMigration(xnsTarget)
	migFailed.Name = "failed"
	migFailed.Status.Phase = infrav1beta1.MigrationPhaseFailed
	migFailed.Status.Conditions = refusedOn(consumerKindProvider, xnsSource, "tgt-prov")

	snapRefused := &infrav1beta1.VMSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "refused", Namespace: bpNS}}
	snapRefused.Status.Conditions = refusedOn(consumerKindProvider, cgOwnerNS, "shared")
	snapOther := &infrav1beta1.VMSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: bpNS}}

	c := grantIndexedClient(t, cloneRefused, cloneRunning, cloneDone, migRefused, migFailed, snapRefused, snapOther)
	clones := &VMCloneReconciler{Client: c, Recorder: record.NewFakeRecorder(1)}
	migrations := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(1)}
	snapshots := &VMSnapshotReconciler{Client: c, Recorder: record.NewFakeRecorder(1)}

	refValue := func(kind, ns, name string) string {
		return consumerRefIndexValue(kind, types.NamespacedName{Namespace: ns, Name: name})
	}

	// The object named in the refusal re-drives the refused, unfinished ones.
	assert.Equal(t, []string{xnsSource + "/refused"}, reqNames(clones.clonesForGrantChange(ctx, refValue(consumerKindVMClass, xnsSource, "src-class"))))
	assert.Equal(t, []string{xnsSource + "/refused"}, reqNames(migrations.migrationsForGrantChange(ctx, refValue(consumerKindProvider, xnsSource, "tgt-prov"))))
	assert.Equal(t, []string{bpNS + "/refused"}, reqNames(snapshots.snapshotsForGrantChange(ctx, refValue(consumerKindProvider, cgOwnerNS, "shared"))))

	// A label change on the clone's or migration's own namespace, or its target
	// namespace, re-drives it too.
	for _, ns := range []string{xnsSource, xnsTarget} {
		assert.Equal(t, []string{xnsSource + "/refused"}, reqNames(clones.clonesForGrantChange(ctx, consumerNamespaceIndexValue(ns))), ns)
		assert.Equal(t, []string{xnsSource + "/refused"}, reqNames(migrations.migrationsForGrantChange(ctx, consumerNamespaceIndexValue(ns))), ns)
	}
	assert.Equal(t, []string{bpNS + "/refused"}, reqNames(snapshots.snapshotsForGrantChange(ctx, consumerNamespaceIndexValue(bpNS))))

	// Unrelated changes enqueue nothing.
	unrelated := refValue(consumerKindVMClass, "team-z", "own-class")
	assert.Empty(t, clones.clonesForGrantChange(ctx, unrelated), "a tenant editing its own VMClass re-drives no clone")
	assert.Empty(t, migrations.migrationsForGrantChange(ctx, unrelated))
	assert.Empty(t, snapshots.snapshotsForGrantChange(ctx, unrelated))
	assert.Empty(t, clones.clonesForGrantChange(ctx, refValue(consumerKindProvider, xnsSource, "tgt-prov")),
		"a clone refused on a class is not re-driven by a Provider change")
	assert.Empty(t, clones.clonesForGrantChange(ctx, consumerNamespaceIndexValue("team-z")))
	assert.Empty(t, migrations.migrationsForGrantChange(ctx, consumerNamespaceIndexValue("team-z")))
	assert.Empty(t, snapshots.snapshotsForGrantChange(ctx, consumerNamespaceIndexValue("team-z")))
}
