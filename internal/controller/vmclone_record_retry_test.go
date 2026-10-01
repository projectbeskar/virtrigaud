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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that the record of a clone the provider made survives a
// transport failure of its write (retried within a bound), and that a record
// that still cannot be written is announced, never silently dropped.

// failingRecordClient fails the first failures VMClone status patches with
// err, counting every attempt.
type failingRecordClient struct {
	client.Client
	err      error
	failures int
	attempts int
}

// Status returns a status writer that fails the first patches.
func (c *failingRecordClient) Status() client.SubResourceWriter {
	return &failingRecordWriter{SubResourceWriter: c.Client.Status(), c: c}
}

// failingRecordWriter is failingRecordClient's status writer.
type failingRecordWriter struct {
	client.SubResourceWriter
	c *failingRecordClient
}

// Patch fails while failures remain, then writes.
func (w *failingRecordWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*infrav1beta1.VMClone); ok {
		w.c.attempts++
		if w.c.attempts <= w.c.failures {
			return w.c.err
		}
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// singleHostRecordFixture is a single-host clone of src-vm whose Clone
// succeeds synchronously.
func singleHostRecordFixture(t *testing.T) (*VMCloneReconciler, *infrav1beta1.VMClone, *clonerProvider) {
	t.Helper()
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default", Finalizers: []string{vmCloneFinalizer}},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-target"}}
	r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, runningProvider("default", "prov-1"),
		sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), clone)
	return r, clone, cp
}

func TestVMClone_RecordSurvivesATransportError(t *testing.T) {
	for name, transportErr := range map[string]error{
		"connection refused": &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		"connection reset":   &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
		"unexpected EOF":     fmt.Errorf("patch status: %w", io.ErrUnexpectedEOF),
		"request deadline":   fmt.Errorf("patch status: %w", context.DeadlineExceeded),
		"server unavailable": apierrors.NewServiceUnavailable("apiserver restarting"),
	} {
		t.Run(name, func(t *testing.T) {
			r, clone, cp := singleHostRecordFixture(t)
			fc := &failingRecordClient{Client: r.Client, err: transportErr, failures: 1}
			r.Client = fc

			reconcileClone(t, r, clone, 1)
			require.Equal(t, 1, cp.cloneCnt)
			assert.Equal(t, 2, fc.attempts, "the record was retried after the transport error")
			assert.Equal(t, "default.clone-target", getClone(t, r, clone).Status.TargetVMID, "and it landed")

			reconcileClone(t, r, clone, 2)
			assert.Equal(t, 1, cp.cloneCnt, "no second Clone")
			assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
			assert.NotContains(t, strings.Join(recordedEvents(t, r), "\n"), cloneReasonRecordNotSaved)
		})
	}
}

// TestVMClone_RecordNotSavedIsAnnounced: a record that cannot be written (here
// refused as Forbidden, which is never retried) is announced with a
// CloneRecordNotSaved Warning naming only the target VM ID.
func TestVMClone_RecordNotSavedIsAnnounced(t *testing.T) {
	r, clone, cp := singleHostRecordFixture(t)
	fc := &failingRecordClient{Client: r.Client, failures: 100,
		err: apierrors.NewForbidden(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmclones/status"}, "clone-1",
			errors.New("RBAC: access denied"))}
	r.Client = fc

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.Error(t, err)
	assert.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, 1, fc.attempts, "Forbidden is never retried")
	evs := strings.Join(recordedEvents(t, r), "\n")
	assert.Contains(t, evs, "Warning "+cloneReasonRecordNotSaved)
	assert.Contains(t, evs, `"default.clone-target"`)
	assert.Contains(t, evs, "a copy may exist on the host")
	assert.NotContains(t, evs, "RBAC", "no API or provider text in the event")
}

// TestIsRetriableRecordError pins what the record write retries.
func TestIsRetriableRecordError(t *testing.T) {
	ctx := context.Background()
	gr := schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmclones"}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"conflict":            {apierrors.NewConflict(gr, "c", errors.New("x")), true},
		"server timeout":      {apierrors.NewServerTimeout(gr, "patch", 1), true},
		"too many requests":   {apierrors.NewTooManyRequests("slow down", 1), true},
		"internal":            {apierrors.NewInternalError(errors.New("x")), true},
		"connection refused":  {&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		"connection reset":    {&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		"EOF":                 {io.EOF, true},
		"request deadline":    {context.DeadlineExceeded, true},
		"net timeout":         {&net.DNSError{Err: "i/o timeout", IsTimeout: true}, true},
		"invalid (test fail)": {apierrors.NewInvalid(schema.GroupKind{Kind: "VMClone"}, "c", nil), false},
		"not found":           {apierrors.NewNotFound(gr, "c"), false},
		"forbidden":           {apierrors.NewForbidden(gr, "c", errors.New("x")), false},
		"unauthorized":        {apierrors.NewUnauthorized("x"), false},
		"bad request":         {apierrors.NewBadRequest("x"), false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, isRetriableRecordError(ctx, tc.err))
		})
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	assert.False(t, isRetriableRecordError(done, context.DeadlineExceeded), "never once the reconcile's own context is done")
	assert.False(t, isRetriableRecordError(done, apierrors.NewConflict(gr, "c", errors.New("x"))))
}
