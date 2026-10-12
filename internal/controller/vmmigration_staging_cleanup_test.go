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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// Values that name the staging location or a credential: none may reach a
// status or an event (ADR-0011).
const (
	stagingTestBucket   = "tenant-staging-bucket"
	stagingTestEndpoint = "http://10.0.0.5:9000"
	stagingTestSecret   = "s3-creds"
	stagingTestKeyValue = "AKIA-SECRET-ACCESS"
)

// stagingProvider is a provider whose DeleteStagedDisk and GetCapabilities
// answer from a script; it counts staged-disk deletes and snapshot deletes.
// It is safe for concurrent use.
type stagingProvider struct {
	stubProvider
	mu       sync.Mutex
	backends []string
	capsErr  error
	outcome  contracts.StagedDiskDeleteOutcome
	err      error
	lastReq  contracts.StagedDiskDeleteRequest

	calls           atomic.Int32
	snapshotDeletes atomic.Int32
}

func (p *stagingProvider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return contracts.Capabilities{SupportedStagedDiskDeleteBackends: p.backends}, p.capsErr
}

func (p *stagingProvider) DeleteStagedDisk(_ context.Context, req contracts.StagedDiskDeleteRequest) (contracts.StagedDiskDeleteOutcome, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastReq = req
	return p.outcome, p.err
}

func (p *stagingProvider) SnapshotDelete(context.Context, contracts.VMRef, string) (string, error) {
	p.snapshotDeletes.Add(1)
	return "", nil
}

// lastRequest returns the last DeleteStagedDisk request.
func (p *stagingProvider) lastRequest() contracts.StagedDiskDeleteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq
}

// answer scripts the next DeleteStagedDisk answers.
func (p *stagingProvider) answer(outcome contracts.StagedDiskDeleteOutcome, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcome, p.err = outcome, err
}

// newStagingProvider supports nfs and s3 and deletes successfully.
func newStagingProvider() *stagingProvider {
	return &stagingProvider{backends: []string{storagemigration.BackendNFS, storagemigration.BackendS3}, outcome: contracts.StagedDiskDeleted}
}

func stagingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, infrav1beta1.AddToScheme(s))
	return s
}

// stagingFixture is an s3 migration in phase whose export was sent
// (StagingCleanup=Unknown), with its source VM, source and target Providers,
// and the S3 credentials Secret.
func stagingFixture(phase infrav1beta1.MigrationPhase) (*infrav1beta1.VMMigration, []client.Object) {
	vm, srcProvider, m := snapshotFixture()
	m.Status.SnapshotID = ""
	m.Finalizers = []string{infrav1beta1.VMMigrationFinalizer}
	m.Status.Phase = phase
	m.Spec.Storage = &infrav1beta1.MigrationStorage{
		Type: storagemigration.BackendS3,
		S3: &infrav1beta1.S3StorageConfig{
			Bucket:               stagingTestBucket,
			Endpoint:             stagingTestEndpoint,
			Prefix:               "tenant-prefix",
			CredentialsSecretRef: infrav1beta1.ObjectRef{Name: stagingTestSecret},
			UsePathStyle:         true,
		},
	}
	target := &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default", UID: "uid-target-1"}}
	// The fixture's s3 options always encode, so the digest cannot fail.
	digest, _ := stagingDigest(m, target)
	markStagedDiskMayExist(m, digest)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: stagingTestSecret, Namespace: "default"},
		Data: map[string][]byte{
			storagemigration.CredKeyAccessKeyID:     []byte("AKID"),
			storagemigration.CredKeySecretAccessKey: []byte(stagingTestKeyValue),
		},
	}
	return m, []client.Object{vm, srcProvider, target, secret}
}

// stagingHarness is a VMMigrationReconciler over a fake client whose provider
// resolution returns prov.
type stagingHarness struct {
	r    *VMMigrationReconciler
	c    client.Client
	rec  *record.FakeRecorder
	prov *stagingProvider
}

func newStagingHarness(t *testing.T, prov *stagingProvider, m *infrav1beta1.VMMigration, objs ...client.Object) *stagingHarness {
	t.Helper()
	scheme := stagingScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, m)...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		Build()
	h := &stagingHarness{c: c, rec: record.NewFakeRecorder(50), prov: prov}
	h.r = &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: h.rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}
	return h
}

func (h *stagingHarness) reconcile(t *testing.T, m *infrav1beta1.VMMigration) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	require.NoError(t, err)
	return res
}

func (h *stagingHarness) get(t *testing.T, m *infrav1beta1.VMMigration) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(m), got); err != nil {
		return nil
	}
	return got
}

func stagingCondition(m *infrav1beta1.VMMigration) *metav1.Condition {
	return meta.FindStatusCondition(m.Status.Conditions, migrationConditionStagingCleanup)
}

// assertNoLocationLeak fails when any status message or event names the
// staging location or a credential.
func assertNoLocationLeak(t *testing.T, m *infrav1beta1.VMMigration, events []string) {
	t.Helper()
	texts := append([]string{m.Status.Message}, events...)
	for _, c := range m.Status.Conditions {
		texts = append(texts, c.Message)
	}
	for _, text := range texts {
		for _, secret := range []string{stagingTestBucket, stagingTestEndpoint, "10.0.0.5", "tenant-prefix", "vmmigrations/",
			stagingTestKeyValue, stagingTestSecret} {
			assert.NotContains(t, text, secret, "a status message or event names the staging location or a credential")
		}
	}
}

// TestStagingCleanupWanted_DecisionTable pins ADR-0011 D3.
func TestStagingCleanupWanted_DecisionTable(t *testing.T) {
	triggers := map[string]stagingCleanupTrigger{
		"ready": stagingTriggerReady, "terminal failure": stagingTriggerTerminalFailure, "deletion": stagingTriggerDeletion,
	}
	want := map[string]map[string]bool{
		infrav1beta1.CleanupPolicyAlways:    {"ready": true, "terminal failure": true, "deletion": true},
		infrav1beta1.CleanupPolicyOnSuccess: {"ready": true, "terminal failure": false, "deletion": true},
		"":                                  {"ready": true, "terminal failure": false, "deletion": true}, // default OnSuccess
		infrav1beta1.CleanupPolicyNever:     {"ready": false, "terminal failure": false, "deletion": false},
	}
	for policy, byTrigger := range want {
		for name, trigger := range triggers {
			m, _ := stagingFixture(infrav1beta1.MigrationPhaseReady)
			m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: policy}
			assert.Equal(t, byTrigger[name], stagingCleanupWanted(m, trigger), "policy %q at %s", policy, name)
		}
	}

	// Nothing staged outside the cluster, or nothing left: never.
	for name, mutate := range map[string]func(m *infrav1beta1.VMMigration){
		"pvc backend": func(m *infrav1beta1.VMMigration) {
			m.Spec.Storage = &infrav1beta1.MigrationStorage{Type: storagemigration.BackendPVC}
		},
		"no storage": func(m *infrav1beta1.VMMigration) { m.Spec.Storage = nil },
		"no export sent": func(m *infrav1beta1.VMMigration) {
			meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionStagingCleanup)
		},
		"already deleted": func(m *infrav1beta1.VMMigration) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionStagingCleanup,
				Status: metav1.ConditionTrue, Reason: migrationReasonStagedDiskDeleted})
		},
	} {
		m, _ := stagingFixture(infrav1beta1.MigrationPhaseReady)
		mutate(m)
		for tn, trigger := range triggers {
			assert.False(t, stagingCleanupWanted(m, trigger), "%s at %s", name, tn)
		}
	}

	// A held delete is still wanted; a migration from before the condition
	// whose export completed is too (legacy).
	m, _ := stagingFixture(infrav1beta1.MigrationPhaseReady)
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionStagingCleanup,
		Status: metav1.ConditionFalse, Reason: migrationReasonStagingCleanupPending})
	assert.True(t, stagingCleanupWanted(m, stagingTriggerReady))
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionStagingCleanup)
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: infrav1beta1.VMMigrationConditionExporting,
		Status: metav1.ConditionTrue, Reason: "ExportComplete"})
	assert.True(t, stagingCleanupWanted(m, stagingTriggerReady), "a pre-ADR-0011 migration whose export completed")
}

// TestReadyDeletesStagedDisk: a succeeded s3 migration asks its TARGET
// provider to delete the staged disk — naming the migration, never a location
// — records StagingCleanup=True with one Normal event, and sends nothing on
// the next reconcile. The source snapshot is deleted in the same pass: the
// former manager-side PVC cleanup failed for every migration with
// spec.storage and blocked it.
func TestReadyDeletesStagedDisk(t *testing.T) {
	prov := newStagingProvider()
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	m.Status.SnapshotID = "snap-abc"
	markPostMigrationCleanupPending(m) // completed by this manager
	h := newStagingHarness(t, prov, m, objs...)

	res := h.reconcile(t, m)
	assert.Equal(t, ctrl.Result{}, res)
	require.EqualValues(t, 1, prov.calls.Load())
	req := prov.lastRequest()
	assert.Equal(t, contracts.ObjectIdentity{UID: "uid-snap-1", Namespace: "default", Name: "snap-migration"}, req.Migration)
	assert.Equal(t, storagemigration.StagingStageExport, req.Stage)
	assert.Equal(t, storagemigration.BackendS3, req.BackendType)
	assert.Contains(t, req.StorageOptionsJSON, stagingTestBucket, "the provider gets the options to reach the backend")
	assert.Equal(t, stagingTestKeyValue, req.Credentials[storagemigration.CredKeySecretAccessKey])

	got := h.get(t, m)
	c := stagingCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, migrationReasonStagedDiskDeleted, c.Reason)
	assert.EqualValues(t, 1, prov.snapshotDeletes.Load(), "the source snapshot is deleted too")
	assert.Empty(t, got.Status.SnapshotID)
	require.NotNil(t, got.Status.StorageInfo)
	assert.True(t, got.Status.StorageInfo.CleanedUp)
	events := drainRecordedEvents(h.rec)
	assert.Len(t, filterEvents(events, migrationReasonStagedDiskDeleted), 1)
	assertNoLocationLeak(t, got, events)

	h.reconcile(t, got)
	assert.EqualValues(t, 1, prov.calls.Load(), "nothing left to delete")
	assert.Empty(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagedDiskDeleted))
}

// TestReadyEmptiedStagedDisk: an nfs target that can only truncate reports it
// as StagedDiskEmptied.
func TestReadyEmptiedStagedDisk(t *testing.T) {
	prov := newStagingProvider()
	prov.answer(contracts.StagedDiskEmptied, nil)
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	h := newStagingHarness(t, prov, m, objs...)
	h.reconcile(t, m)
	c := stagingCondition(h.get(t, m))
	require.NotNil(t, c)
	assert.Equal(t, migrationReasonStagedDiskEmptied, c.Reason)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
}

// TestReadyHoldsAFailedStagedDiskDelete: a retryable failure is held —
// StagingCleanup=False with the constant message, one Warning, the blocked-VM
// backoff — and the reconcile the hold's own write triggers sends nothing.
func TestReadyHoldsAFailedStagedDiskDelete(t *testing.T) {
	prov := newStagingProvider()
	prov.answer("", contracts.NewRetryableError("deleteStagedDisk: s3 stat of s3://"+stagingTestBucket+"/x failed", nil))
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	h := newStagingHarness(t, prov, m, objs...)

	res := h.reconcile(t, m)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	got := h.get(t, m)
	c := stagingCondition(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, migrationReasonStagingCleanupPending, c.Reason)
	assert.Equal(t, migrationStagingCleanupPendingMessage, c.Message)
	events := drainRecordedEvents(h.rec)
	assert.Len(t, filterEvents(events, migrationReasonStagingCleanupPending), 1)
	assertNoLocationLeak(t, got, events)

	// The reconcile the hold's own status write triggers: not due yet.
	res = h.reconcile(t, got)
	assert.EqualValues(t, 1, prov.calls.Load(), "no delete ahead of the backoff")
	assert.Greater(t, res.RequeueAfter, time.Duration(0))
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMin)

	// Due again and still failing: no second Warning, no status write churn.
	aged := h.get(t, got)
	ageStagingHold(t, h, aged, 2*time.Minute)
	res = h.reconcile(t, aged)
	assert.EqualValues(t, 2, prov.calls.Load())
	assert.Greater(t, res.RequeueAfter, blockedRetryMin, "the backoff grows")
	assert.Empty(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagingCleanupPending), "one Warning per hold")

	// The provider recovers: done.
	prov.answer(contracts.StagedDiskNotFound, nil)
	again := h.get(t, aged)
	ageStagingHold(t, h, again, 10*time.Minute)
	h.reconcile(t, again)
	c = stagingCondition(h.get(t, again))
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, migrationReasonStagedDiskDeleted, c.Reason)
}

// ageStagingHold moves the stored hold's start back by d.
func ageStagingHold(t *testing.T, h *stagingHarness, m *infrav1beta1.VMMigration, d time.Duration) {
	t.Helper()
	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == migrationConditionStagingCleanup {
			m.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-d))
		}
	}
	require.NoError(t, h.c.Status().Update(context.Background(), m))
}

// TestReadyLeavesBehindWhatCanNeverBeDeleted: a provider that cannot delete
// (no capability, Unimplemented, a refused request), a target Provider that is
// gone, a Secret that is gone, or a staging host the policy now refuses is
// recorded once as StagedDiskLeftBehind (Warning) and never retried.
func TestReadyLeavesBehindWhatCanNeverBeDeleted(t *testing.T) {
	for name, tc := range map[string]struct {
		setup    func(p *stagingProvider, m *infrav1beta1.VMMigration, objs []client.Object) []client.Object
		wantRPCs int32
	}{
		"provider refuses the request": {wantRPCs: 1, setup: func(p *stagingProvider, _ *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			p.answer("", contracts.NewInvalidSpecError("deleteStagedDisk: bad", nil))
			return o
		}},
		"target Provider gone": {setup: func(_ *stagingProvider, _ *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			return withoutObject(o, "target-provider")
		}},
		"credentials Secret gone": {setup: func(_ *stagingProvider, _ *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			return withoutObject(o, stagingTestSecret)
		}},
		"staging host now refused by the SSRF policy": {setup: func(_ *stagingProvider, m *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			m.Spec.Storage.S3.Endpoint = "http://169.254.169.254"
			return o
		}},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newStagingProvider()
			m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
			objs = tc.setup(prov, m, objs)
			h := newStagingHarness(t, prov, m, objs...)

			assert.Equal(t, ctrl.Result{}, h.reconcile(t, m))
			assert.Equal(t, tc.wantRPCs, prov.calls.Load())
			got := h.get(t, m)
			c := stagingCondition(got)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, migrationReasonStagedDiskLeftBehind, c.Reason)
			events := drainRecordedEvents(h.rec)
			assert.Len(t, filterEvents(events, migrationReasonStagedDiskLeftBehind), 1)
			assertNoLocationLeak(t, got, events)

			h.reconcile(t, got)
			assert.Equal(t, tc.wantRPCs, prov.calls.Load(), "recorded once, never retried")
			assert.Empty(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagedDiskLeftBehind))
		})
	}
}

// TestCapabilityQueryFailureIsHeld: a capability query that fails (the
// provider is unreachable) is held, not recorded as left behind.
func TestCapabilityQueryFailureIsHeld(t *testing.T) {
	prov := newStagingProvider()
	prov.capsErr = contracts.NewRetryableError("get capabilities: unavailable", nil)
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	h := newStagingHarness(t, prov, m, objs...)
	h.reconcile(t, m)
	c := stagingCondition(h.get(t, m))
	require.NotNil(t, c)
	assert.Equal(t, migrationReasonStagingCleanupPending, c.Reason)
	assert.EqualValues(t, 0, prov.calls.Load())
}

// TestCleanupPolicyNeverRetainsTheStagedDisk: no RPC; StagedDiskRetained.
func TestCleanupPolicyNeverRetainsTheStagedDisk(t *testing.T) {
	prov := newStagingProvider()
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
	h := newStagingHarness(t, prov, m, objs...)
	h.reconcile(t, m)
	assert.EqualValues(t, 0, prov.calls.Load())
	c := stagingCondition(h.get(t, m))
	require.NotNil(t, c)
	assert.Equal(t, migrationReasonStagedDiskRetained, c.Reason)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
}

// TestTerminalFailureFollowsCleanupPolicy: a migration that failed for good
// keeps its staged disk with OnSuccess (until it is deleted) and deletes it
// with Always.
func TestTerminalFailureFollowsCleanupPolicy(t *testing.T) {
	for policy, wantCalls := range map[string]int32{
		infrav1beta1.CleanupPolicyOnSuccess: 0,
		infrav1beta1.CleanupPolicyAlways:    1,
	} {
		t.Run(policy, func(t *testing.T) {
			prov := newStagingProvider()
			m, objs := stagingFixture(infrav1beta1.MigrationPhaseFailed)
			m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: policy}
			h := newStagingHarness(t, prov, m, objs...)
			h.reconcile(t, m)
			assert.Equal(t, wantCalls, prov.calls.Load())
			c := stagingCondition(h.get(t, m))
			require.NotNil(t, c)
			if wantCalls == 0 {
				assert.Equal(t, metav1.ConditionUnknown, c.Status, "kept for diagnosis until the VMMigration is deleted")
				assert.True(t, migrationHoldsProvider(h.get(t, m)), "the target Provider is kept for the later delete")
			} else {
				assert.Equal(t, metav1.ConditionTrue, c.Status)
				assert.False(t, migrationHoldsProvider(h.get(t, m)))
			}
		})
	}
}

// TestDeletionHoldsReleasesAndForceDeletes drives the finalizer: a failing
// delete keeps the VMMigration (held, backoff), a recovered provider releases
// it, and force-delete releases it at once with a StagedDiskLeftBehind Warning.
func TestDeletionHoldsReleasesAndForceDeletes(t *testing.T) {
	deleting := func(m *infrav1beta1.VMMigration) {
		now := metav1.Now()
		m.DeletionTimestamp = &now
	}

	t.Run("held, then released when the provider recovers", func(t *testing.T) {
		prov := newStagingProvider()
		prov.answer("", contracts.NewRetryableError("unavailable", nil))
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		deleting(m)
		h := newStagingHarness(t, prov, m, objs...)

		res := h.reconcile(t, m)
		assert.Equal(t, blockedRetryMin, res.RequeueAfter)
		got := h.get(t, m)
		require.NotNil(t, got, "the finalizer is kept")
		assert.Contains(t, got.Finalizers, infrav1beta1.VMMigrationFinalizer)
		assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(got).Reason)

		prov.answer(contracts.StagedDiskDeleted, nil)
		ageStagingHold(t, h, got, time.Minute)
		h.reconcile(t, got)
		assert.Nil(t, h.get(t, m), "released once the staged disk is deleted")
		assert.EqualValues(t, 2, prov.calls.Load())
	})

	t.Run("force-delete releases with a Warning", func(t *testing.T) {
		prov := newStagingProvider()
		prov.answer("", contracts.NewRetryableError("unavailable", nil))
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		deleting(m)
		m.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		events := drainRecordedEvents(h.rec)
		left := filterEvents(events, migrationReasonStagedDiskLeftBehind)
		require.Len(t, left, 1)
		assert.Contains(t, left[0], forceDeleteAnnotation)
		for _, e := range events {
			assert.NotContains(t, e, stagingTestBucket)
		}
	})

	t.Run("force-delete during a hold that is not due yet", func(t *testing.T) {
		prov := newStagingProvider()
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		deleting(m)
		m.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionStagingCleanup,
			Status: metav1.ConditionFalse, Reason: migrationReasonStagingCleanupPending, Message: migrationStagingCleanupPendingMessage})
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		assert.EqualValues(t, 0, prov.calls.Load(), "acted on at once, no extra RPC")
	})

	t.Run("an older provider holds, then releases with a Warning after the grace window", func(t *testing.T) {
		prov := newStagingProvider()
		prov.backends = nil
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		deleting(m)
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		got := h.get(t, m)
		require.NotNil(t, got, "held while the provider may still be rolled to a capable image")
		assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(got).Reason)
		ageStagingHold(t, h, got, stagedDiskUnsupportedGrace+time.Minute)
		h.reconcile(t, got)
		assert.Nil(t, h.get(t, m))
		assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagedDiskLeftBehind), 1)
	})

	t.Run("cleanupPolicy Never releases without an RPC", func(t *testing.T) {
		prov := newStagingProvider()
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseFailed)
		deleting(m)
		m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		assert.EqualValues(t, 0, prov.calls.Load())
	})
}

// TestMigrationHoldsProviderWhileStagingCleanupIsPending pins ADR-0011 D8.
func TestMigrationHoldsProviderWhileStagingCleanupIsPending(t *testing.T) {
	m, _ := stagingFixture(infrav1beta1.MigrationPhaseReady)
	assert.True(t, migrationHoldsProvider(m), "Ready with a staged disk still to delete")
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: migrationConditionStagingCleanup,
		Status: metav1.ConditionTrue, Reason: migrationReasonStagedDiskDeleted})
	assert.False(t, migrationHoldsProvider(m), "Ready and cleaned up")
	m.Spec.Storage = &infrav1beta1.MigrationStorage{Type: storagemigration.BackendPVC}
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionStagingCleanup)
	assert.False(t, migrationHoldsProvider(m), "a pvc migration stages nothing outside the cluster")
}

// TestMarkStagedDiskMayExist: only nfs / s3 migrations are marked, once.
func TestMarkStagedDiskMayExist(t *testing.T) {
	m, _ := stagingFixture(infrav1beta1.MigrationPhaseExporting)
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionStagingCleanup)
	assert.True(t, markStagedDiskMayExist(m, "sha256:x"))
	assert.False(t, markStagedDiskMayExist(m, "sha256:x"), "a second mark changes nothing")
	assert.True(t, markStagedDiskMayExist(m, "sha256:y"), "a later export records its own digest")
	c := stagingCondition(m)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionUnknown, c.Status)
	assert.Equal(t, migrationReasonStagedDiskMayExist, c.Reason)

	m.Spec.Storage = &infrav1beta1.MigrationStorage{Type: storagemigration.BackendPVC}
	meta.RemoveStatusCondition(&m.Status.Conditions, migrationConditionStagingCleanup)
	assert.False(t, markStagedDiskMayExist(m, "sha256:x"))
	assert.Nil(t, stagingCondition(m))
}

// withoutObject drops the object named name from objs.
func withoutObject(objs []client.Object, name string) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if o.GetName() != name {
			out = append(out, o)
		}
	}
	return out
}

// filterEvents returns the recorded events whose reason is reason.
func filterEvents(events []string, reason string) []string {
	var out []string
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}

// markObservingExporter is a provider whose ExportDisk reads the STORED
// migration (as another reader would after a crash) and records whether its
// StagingCleanup marker was already persisted when the export was sent.
type markObservingExporter struct {
	stubProvider
	c         client.Client
	key       client.ObjectKey
	exportErr error
	sawMarker atomic.Bool
	exports   atomic.Int32
}

func (p *markObservingExporter) ExportDisk(ctx context.Context, _ contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.exports.Add(1)
	stored := &infrav1beta1.VMMigration{}
	if err := p.c.Get(ctx, p.key, stored); err == nil {
		if c := stagingCondition(stored); c != nil && c.Status == metav1.ConditionUnknown {
			p.sawMarker.Store(true)
		}
	}
	if p.exportErr != nil {
		return contracts.ExportDiskResponse{}, p.exportErr
	}
	return contracts.ExportDiskResponse{ExportId: "export-1"}, nil
}

// TestExportPersistsTheStagingMarkerBeforeTheRPC: the StagingCleanup marker
// is stored BEFORE ExportDisk is sent, so an export that fails after writing
// part of the object is still cleaned up; and the ExportComplete event no
// longer names the destination.
func TestExportPersistsTheStagingMarkerBeforeTheRPC(t *testing.T) {
	for name, exportErr := range map[string]error{
		"successful export": nil,
		"failed export":     contracts.NewRetryableError("exportDisk: connection reset", nil),
	} {
		t.Run(name, func(t *testing.T) {
			vm, srcProvider, m := raceMigrationFixture()
			m.Spec.Storage = &infrav1beta1.MigrationStorage{
				Type: storagemigration.BackendNFS,
				NFS:  &infrav1beta1.NFSStorageConfig{Server: "10.0.0.7", Export: "/exports/tenant-a"},
			}
			m.Status.StoragePVCName = ""
			prov := &markObservingExporter{key: client.ObjectKeyFromObject(m), exportErr: exportErr}
			r, c := newRaceReconciler(t, prov, vm, srcProvider, m, raceTargetProvider())
			prov.c = c

			_, err := r.handleExportingPhase(context.Background(), m)
			require.NoError(t, err)
			assert.EqualValues(t, 1, prov.exports.Load())
			assert.True(t, prov.sawMarker.Load(), "the marker was stored before the export was sent")

			got := &infrav1beta1.VMMigration{}
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
			assert.True(t, stagedDiskMayExist(got), "the staged disk is tracked whatever the export's outcome")
			events := drainRecordedEvents(fakeRecorderOf(t, r.Recorder))
			for _, e := range events {
				assert.NotContains(t, e, "nfs://")
				assert.NotContains(t, e, "/exports/tenant-a")
			}
		})
	}
}

// TestExportOfAPVCMigrationIsNotMarked: a pvc migration stages nothing
// outside the cluster and gets no StagingCleanup condition.
func TestExportOfAPVCMigrationIsNotMarked(t *testing.T) {
	vm, srcProvider, m := raceMigrationFixture()
	prov := &markObservingExporter{key: client.ObjectKeyFromObject(m)}
	r, c := newRaceReconciler(t, prov, vm, srcProvider, m, raceTargetProvider())
	prov.c = c
	_, err := r.handleExportingPhase(context.Background(), m)
	require.NoError(t, err)
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	assert.Nil(t, stagingCondition(got))
}

// urlRecordingExporter records the ExportDisk destination.
type urlRecordingExporter struct {
	stubProvider
	mu  sync.Mutex
	url string
}

func (p *urlRecordingExporter) ExportDisk(_ context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.url = req.DestinationURL
	return contracts.ExportDiskResponse{ExportId: "export-1"}, nil
}

// TestExportRecordsTheStagingLayout (review D3): a fresh nfs migration is
// exported under layout v2 (unambiguous name) and records it; one an earlier
// manager may already have staged (Exporting=True, nothing recorded) keeps the
// legacy v1 name, so its import and delete still find it. The import URL is
// built from the recorded layout.
func TestExportRecordsTheStagingLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		legacy     bool
		wantLayout string
		wantFile   string
	}{
		"new migration":             {false, storagemigration.StagingLayoutV2, "vmmigrations_default_race-migration_export.qcow2"},
		"exported by older manager": {true, storagemigration.StagingLayoutV1, "vmmigrations-default-race-migration-export.qcow2"},
	} {
		t.Run(name, func(t *testing.T) {
			vm, srcProvider, m := raceMigrationFixture()
			m.Spec.Storage = &infrav1beta1.MigrationStorage{
				Type: storagemigration.BackendNFS,
				NFS:  &infrav1beta1.NFSStorageConfig{Server: "10.0.0.7", Export: "/exports"},
			}
			m.Status.StoragePVCName = ""
			if tc.legacy {
				// A retried export of a migration whose first export ran
				// under the older manager: its object is under the legacy name.
				meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: infrav1beta1.VMMigrationConditionExporting,
					Status: metav1.ConditionTrue, Reason: "ExportComplete"})
				m.Status.ExportID = ""
			}
			prov := &urlRecordingExporter{}
			r, c := newRaceReconciler(t, prov, vm, srcProvider, m, raceTargetProvider())
			if tc.legacy {
				// exportAlreadyDone would skip the RPC; drive the marker directly.
				markStagedDiskMayExist(m, "sha256:legacy-retry")
			} else {
				_, err := r.handleExportingPhase(context.Background(), m)
				require.NoError(t, err)
				got := &infrav1beta1.VMMigration{}
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
				m = got
				assert.True(t, strings.HasSuffix(prov.url, "/exports/"+tc.wantFile), "export URL %q", prov.url)
			}
			require.NotNil(t, m.Status.StorageInfo)
			assert.Equal(t, tc.wantLayout, m.Status.StorageInfo.StagingLayout)
			importURL, err := r.generateStorageURL(context.Background(), m, "export")
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(importURL, "/exports/"+tc.wantFile), "import URL %q", importURL)

			// The recorded layout is sticky: marking again never changes it.
			markStagedDiskMayExist(m, "sha256:again")
			assert.Equal(t, tc.wantLayout, m.Status.StorageInfo.StagingLayout)
		})
	}
}

// TestDeleteSendsTheRecordedLayout: the delete names the object with the
// layout recorded at export; a migration with none recorded sends v1.
func TestDeleteSendsTheRecordedLayout(t *testing.T) {
	for recorded, want := range map[string]string{
		storagemigration.StagingLayoutV2: storagemigration.StagingLayoutV2,
		"":                               storagemigration.StagingLayoutV1,
	} {
		prov := newStagingProvider()
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		m.Status.StorageInfo.StagingLayout = recorded
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		require.EqualValues(t, 1, prov.calls.Load())
		assert.Equal(t, want, prov.lastRequest().Layout)
	}
}

// TestDeleteNamesTheTargetVMOnlyWhenItsImportMayHaveStaged: DeleteStagedDisk
// names the target VM (so Proxmox also removes its node-side import stage)
// only for a migration that sent an import, and never while another in-flight
// VMMigration targets the same VM (its Create may be about to read that
// stage). A Ready or Failed one, or one in another target, does not count.
func TestDeleteNamesTheTargetVMOnlyWhenItsImportMayHaveStaged(t *testing.T) {
	want := contracts.ObjectIdentity{Namespace: "default", Name: "target-vm"}
	other := func(name string, phase infrav1beta1.MigrationPhase, targetName string) *infrav1beta1.VMMigration {
		return &infrav1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
			Spec: infrav1beta1.VMMigrationSpec{
				Source: infrav1beta1.MigrationSource{VMRef: infrav1beta1.LocalObjectReference{Name: "other-source"}},
				Target: infrav1beta1.MigrationTarget{Name: targetName, ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"}},
			},
			Status: infrav1beta1.VMMigrationStatus{Phase: phase},
		}
	}
	for name, tc := range map[string]struct {
		phase  infrav1beta1.MigrationPhase
		mutate func(m *infrav1beta1.VMMigration)
		others []client.Object
		want   contracts.ObjectIdentity
	}{
		"ready":                                {phase: infrav1beta1.MigrationPhaseReady, want: want},
		"failed after the import (import id)":  {phase: infrav1beta1.MigrationPhaseFailed, mutate: func(m *infrav1beta1.VMMigration) { m.Status.ImportID = "imp-1" }, want: want},
		"failed before the import":             {phase: infrav1beta1.MigrationPhaseFailed},
		"another migration is creating it":     {phase: infrav1beta1.MigrationPhaseReady, others: []client.Object{other("again", infrav1beta1.MigrationPhaseCreating, "target-vm")}},
		"another migration is importing it":    {phase: infrav1beta1.MigrationPhaseReady, others: []client.Object{other("again", infrav1beta1.MigrationPhaseImporting, "target-vm")}},
		"a failed or ready one does not count": {phase: infrav1beta1.MigrationPhaseReady, others: []client.Object{other("f", infrav1beta1.MigrationPhaseFailed, "target-vm"), other("r", infrav1beta1.MigrationPhaseReady, "target-vm")}, want: want},
		"one for another VM does not count":    {phase: infrav1beta1.MigrationPhaseReady, others: []client.Object{other("elsewhere", infrav1beta1.MigrationPhaseCreating, "another-vm")}, want: want},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newStagingProvider()
			m, objs := stagingFixture(tc.phase)
			m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyAlways}
			if tc.mutate != nil {
				tc.mutate(m)
			}
			h := newStagingHarness(t, prov, m, append(objs, tc.others...)...)
			h.reconcile(t, m)
			require.EqualValues(t, 1, prov.calls.Load())
			assert.Equal(t, tc.want, prov.lastRequest().TargetVM)
		})
	}
}

// raceTargetProvider is raceMigrationFixture's target Provider.
func raceTargetProvider() *infrav1beta1.Provider {
	return &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default", UID: "uid-race-target"}}
}

// TestStagedDiskDeleteFollowsOnlyTheExportedLocation (review M2): a delete is
// sent only while the current spec yields the digest recorded at export. A
// changed spec.storage or target Provider (a re-created one has another UID)
// records the staged disk as left behind, and no delete is aimed elsewhere.
// A migration exported by an older manager (no digest) is deleted best
// effort, but "not found" is never reported as deleted.
func TestStagedDiskDeleteFollowsOnlyTheExportedLocation(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate     func(m *infrav1beta1.VMMigration, objs []client.Object) []client.Object
		outcome    contracts.StagedDiskDeleteOutcome
		wantCalls  int32
		wantReason string
	}{
		"bucket edited after the export": {mutate: func(m *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			m.Spec.Storage.S3.Bucket = "another-bucket"
			return o
		}, wantReason: migrationReasonStagedDiskLeftBehind},
		"prefix edited after the export": {mutate: func(m *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			m.Spec.Storage.S3.Prefix = "elsewhere"
			return o
		}, wantReason: migrationReasonStagedDiskLeftBehind},
		"target Provider re-created": {mutate: func(_ *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			o = withoutObject(o, "target-provider")
			return append(o, &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default", UID: "uid-target-2"}})
		}, wantReason: migrationReasonStagedDiskLeftBehind},
		"unchanged spec, object already gone": {outcome: contracts.StagedDiskNotFound, wantCalls: 1,
			wantReason: migrationReasonStagedDiskDeleted},
		"older manager's export, not found": {mutate: func(m *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			m.Status.StorageInfo.StagingDigest = ""
			return o
		}, outcome: contracts.StagedDiskNotFound, wantCalls: 1, wantReason: migrationReasonStagedDiskLeftBehind},
		"older manager's export, deleted": {mutate: func(m *infrav1beta1.VMMigration, o []client.Object) []client.Object {
			m.Status.StorageInfo.StagingDigest = ""
			return o
		}, outcome: contracts.StagedDiskDeleted, wantCalls: 1, wantReason: migrationReasonStagedDiskDeleted},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newStagingProvider()
			if tc.outcome != "" {
				prov.answer(tc.outcome, nil)
			}
			m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
			if tc.mutate != nil {
				objs = tc.mutate(m, objs)
			}
			h := newStagingHarness(t, prov, m, objs...)
			h.reconcile(t, m)
			assert.Equal(t, tc.wantCalls, prov.calls.Load())
			c := stagingCondition(h.get(t, m))
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, tc.wantReason, c.Reason)
		})
	}
}

// TestUnsupportedProviderIsHeldThenLeftBehind (review M3b): a target provider
// that cannot delete — an older image without the capability, without this
// backend, or answering Unimplemented — is held (the provider pods may roll
// after the manager), retried, and only recorded as left behind once the hold
// is older than stagedDiskUnsupportedGrace. A provider rolled within the
// window deletes the staged disk.
func TestUnsupportedProviderIsHeldThenLeftBehind(t *testing.T) {
	for name, setup := range map[string]func(p *stagingProvider){
		"no capability":    func(p *stagingProvider) { p.backends = nil },
		"not this backend": func(p *stagingProvider) { p.backends = []string{storagemigration.BackendNFS} },
		"Unimplemented": func(p *stagingProvider) {
			p.answer("", contracts.NewNotSupportedError("deleteStagedDisk: not implemented"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			prov := newStagingProvider()
			setup(prov)
			m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
			h := newStagingHarness(t, prov, m, objs...)

			res := h.reconcile(t, m)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			got := h.get(t, m)
			assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(got).Reason)

			ageStagingHold(t, h, got, stagedDiskUnsupportedGrace-time.Hour)
			h.reconcile(t, got)
			got = h.get(t, m)
			assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(got).Reason, "still within the grace window")

			ageStagingHold(t, h, got, stagedDiskUnsupportedGrace+time.Minute)
			h.reconcile(t, got)
			c := stagingCondition(h.get(t, m))
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, migrationReasonStagedDiskLeftBehind, c.Reason)
			assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagedDiskLeftBehind), 1)
		})
	}

	t.Run("provider rolled within the window", func(t *testing.T) {
		prov := newStagingProvider()
		prov.backends = nil
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
		h := newStagingHarness(t, prov, m, objs...)
		h.reconcile(t, m)
		prov.mu.Lock()
		prov.backends = []string{storagemigration.BackendS3}
		prov.mu.Unlock()
		got := h.get(t, m)
		ageStagingHold(t, h, got, time.Hour)
		h.reconcile(t, got)
		assert.Equal(t, migrationReasonStagedDiskDeleted, stagingCondition(h.get(t, m)).Reason)
	})
}

// TestUnresolvableStagingHostIsHeld (review M3a): a staging host whose name
// cannot be resolved (a DNS failure can be transient) is held, not recorded
// as left behind; only a policy denial is.
func TestUnresolvableStagingHostIsHeld(t *testing.T) {
	prov := newStagingProvider()
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	m.Spec.Storage.S3.Endpoint = "http://staging.does-not-exist.invalid:9000"
	h := newStagingHarness(t, prov, m, objs...)
	h.reconcile(t, m)
	assert.EqualValues(t, 0, prov.calls.Load())
	assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(h.get(t, m)).Reason)
}

// exportTaskProvider is a stagingProvider whose export task is reported
// running until done is set.
type exportTaskProvider struct {
	stagingProvider
	done  atomic.Bool
	polls atomic.Int32
}

func (p *exportTaskProvider) IsTaskComplete(context.Context, string) (bool, error) {
	p.polls.Add(1)
	return p.done.Load(), nil
}

// TestDeletionWaitsForARunningExportTask (review M4): a VMMigration deleted
// while its export task still runs is held — no delete is sent, since the task
// would write the object after it — and the staged disk is deleted once the
// task is finished. force-delete releases it at once.
func TestDeletionWaitsForARunningExportTask(t *testing.T) {
	newDeleting := func(force bool) (*exportTaskProvider, *stagingHarness, *infrav1beta1.VMMigration) {
		prov := &exportTaskProvider{stagingProvider: stagingProvider{
			backends: []string{storagemigration.BackendNFS, storagemigration.BackendS3}, outcome: contracts.StagedDiskDeleted}}
		m, objs := stagingFixture(infrav1beta1.MigrationPhaseExporting)
		m.Status.TaskRef = "export-task-1"
		now := metav1.Now()
		m.DeletionTimestamp = &now
		if force {
			m.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		}
		scheme := stagingScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, m)...).
			WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
		h := &stagingHarness{c: c, rec: record.NewFakeRecorder(50), prov: &prov.stagingProvider}
		h.r = &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: h.rec,
			providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}
		return prov, h, m
	}

	t.Run("held until the task is finished", func(t *testing.T) {
		prov, h, m := newDeleting(false)
		res := h.reconcile(t, m)
		assert.Greater(t, res.RequeueAfter, time.Duration(0))
		got := h.get(t, m)
		require.NotNil(t, got, "the finalizer is kept while the export task runs")
		assert.Equal(t, migrationReasonStagingCleanupPending, stagingCondition(got).Reason)
		assert.EqualValues(t, 0, prov.calls.Load(), "no delete while the export can still write the object")
		assert.EqualValues(t, 1, prov.polls.Load())

		// The reconcile the hold's own write triggers is not due: no poll either.
		h.reconcile(t, got)
		assert.EqualValues(t, 1, prov.polls.Load(), "the export task is not polled ahead of the backoff")
		got = h.get(t, m)

		prov.done.Store(true)
		ageStagingHold(t, h, got, time.Minute)
		h.reconcile(t, got)
		assert.Nil(t, h.get(t, m))
		assert.EqualValues(t, 1, prov.calls.Load())
	})

	t.Run("force-delete releases at once", func(t *testing.T) {
		prov, h, m := newDeleting(true)
		h.reconcile(t, m)
		assert.Nil(t, h.get(t, m))
		assert.EqualValues(t, 0, prov.calls.Load())
		assert.Len(t, filterEvents(drainRecordedEvents(h.rec), migrationReasonStagedDiskLeftBehind), 1)
	})
}

// TestLegacyExportTraceKeepsTheV1Layout (re-review Low a): a migration an
// earlier manager may already have exported — an export id, an Exporting
// condition of any status, or a running export task — but that recorded no
// marker keeps layout v1, so its partial v1 object is overwritten and found
// by its cleanup; a migration with no trace gets v2.
func TestLegacyExportTraceKeepsTheV1Layout(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(m *infrav1beta1.VMMigration)
		want   string
	}{
		"no trace":  {func(*infrav1beta1.VMMigration) {}, storagemigration.StagingLayoutV2},
		"export id": {func(m *infrav1beta1.VMMigration) { m.Status.ExportID = "export-old" }, storagemigration.StagingLayoutV1},
		"failed Exporting condition": {func(m *infrav1beta1.VMMigration) {
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: infrav1beta1.VMMigrationConditionExporting,
				Status: metav1.ConditionFalse, Reason: "ExportFailed"})
		}, storagemigration.StagingLayoutV1},
		"export task": {func(m *infrav1beta1.VMMigration) { m.Status.TaskRef = "task-1" }, storagemigration.StagingLayoutV1},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, m := raceMigrationFixture()
			m.Spec.Storage = &infrav1beta1.MigrationStorage{Type: storagemigration.BackendNFS,
				NFS: &infrav1beta1.NFSStorageConfig{Server: "10.0.0.7", Export: "/exports"}}
			tc.mutate(m)
			require.True(t, markStagedDiskMayExist(m, "sha256:x"))
			assert.Equal(t, tc.want, m.Status.StorageInfo.StagingLayout)
		})
	}
}

// TestStagingDigestIsCanonicalAndVersioned (re-review Low c): the digest is
// computed from a versioned canonical struct of spec fields (pinned here, so
// a change to it is deliberate), not from the wire options JSON; every field
// that decides the location, and the target Provider's UID, changes it.
func TestStagingDigestIsCanonicalAndVersioned(t *testing.T) {
	m, _ := stagingFixture(infrav1beta1.MigrationPhaseReady)
	target := &infrav1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default", UID: "uid-target-1"}}
	d, err := stagingDigest(m, target)
	require.NoError(t, err)
	want, err := json.Marshal(stagingDigestInputV1{Version: stagingDigestVersion, Backend: "s3",
		S3Bucket: stagingTestBucket, S3Endpoint: stagingTestEndpoint, S3Prefix: "tenant-prefix", S3UsePathStyle: true,
		ProviderNamespace: "default", ProviderName: "target-provider", ProviderUID: "uid-target-1"})
	require.NoError(t, err)
	sum := sha256.Sum256(want)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), d)
	assert.Equal(t, `{"v":"staging-digest-v1","backend":"s3","s3Bucket":"tenant-staging-bucket","s3Endpoint":"http://10.0.0.5:9000",`+
		`"s3Region":"","s3Prefix":"tenant-prefix","s3UsePathStyle":true,"nfsServer":"","nfsExport":"","nfsPath":"","nfsUID":null,`+
		`"nfsGID":null,"providerNamespace":"default","providerName":"target-provider","providerUID":"uid-target-1"}`, string(want),
		"the canonical v1 form is pinned: never change it, add a v2")

	for name, mutate := range map[string]func(m *infrav1beta1.VMMigration, p *infrav1beta1.Provider){
		"bucket": func(m *infrav1beta1.VMMigration, _ *infrav1beta1.Provider) { m.Spec.Storage.S3.Bucket = "x" },
		"endpoint": func(m *infrav1beta1.VMMigration, _ *infrav1beta1.Provider) {
			m.Spec.Storage.S3.Endpoint = "http://10.0.0.6"
		},
		"region":       func(m *infrav1beta1.VMMigration, _ *infrav1beta1.Provider) { m.Spec.Storage.S3.Region = "eu" },
		"prefix":       func(m *infrav1beta1.VMMigration, _ *infrav1beta1.Provider) { m.Spec.Storage.S3.Prefix = "p2" },
		"path style":   func(m *infrav1beta1.VMMigration, _ *infrav1beta1.Provider) { m.Spec.Storage.S3.UsePathStyle = false },
		"provider UID": func(_ *infrav1beta1.VMMigration, p *infrav1beta1.Provider) { p.UID = "uid-target-2" },
	} {
		mm, pp := m.DeepCopy(), target.DeepCopy()
		mutate(mm, pp)
		other, err := stagingDigest(mm, pp)
		require.NoError(t, err)
		assert.NotEqual(t, d, other, name)
	}
}
