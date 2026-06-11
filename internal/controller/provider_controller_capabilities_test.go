/*
Copyright 2025.

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

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
)

// TestReconcileReportedCapabilities_NilResolver verifies the best-effort
// contract (issue #176): with no resolver configured the reconciler must NOT
// panic, must set CapabilitiesReported=False with the Unavailable reason, and
// must leave ReportedCapabilities untouched (nil here).
func TestReconcileReportedCapabilities_NilResolver(t *testing.T) {
	r := &ProviderReconciler{} // RemoteResolver is nil
	provider := &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
	}

	r.reconcileReportedCapabilities(context.Background(), provider)

	c := getConditionByType(t, provider.Status.Conditions, providerConditionCapabilitiesReported)
	require.NotNil(t, c, "CapabilitiesReported condition must be set")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, providerReasonCapabilitiesUnavailable, c.Reason)
	assert.Nil(t, provider.Status.ReportedCapabilities,
		"capabilities must not be fabricated when none could be fetched")
}

// TestReconcileReportedCapabilities_ResolveFails verifies that when the
// resolver cannot produce a provider client (here: the Provider has no
// runtime endpoint, so GetProvider errors), the reconciler treats it as
// best-effort: condition False/Unavailable, no panic, no fabricated
// capabilities, and (critically) it does not return or surface an error that
// would fail the reconcile.
func TestReconcileReportedCapabilities_ResolveFails(t *testing.T) {
	// A resolver backed by a nil k8s client and nil breaker registry. The
	// Provider below has no runtime status, so getRemoteProvider short-
	// circuits with "runtime is not ready" before any k8s call — exercising
	// the resolve-failure branch deterministically and offline.
	r := &ProviderReconciler{
		RemoteResolver: remote.NewResolver(nil, nil),
	}
	provider := &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		// No Status.Runtime → resolver returns an error.
	}

	r.reconcileReportedCapabilities(context.Background(), provider)

	c := getConditionByType(t, provider.Status.Conditions, providerConditionCapabilitiesReported)
	require.NotNil(t, c, "CapabilitiesReported condition must be set")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, providerReasonCapabilitiesUnavailable, c.Reason)
	assert.Nil(t, provider.Status.ReportedCapabilities)
}

// TestCapabilitiesToReported verifies the field-for-field mapping from the
// transport-agnostic contracts.Capabilities to the CRD-facing
// v1beta1.ReportedCapabilities (issue #176).
func TestCapabilitiesToReported(t *testing.T) {
	caps := contracts.Capabilities{
		SupportsReconfigureOnline:   true,
		SupportsDiskExpansionOnline: true,
		SupportsSnapshots:           true,
		SupportsMemorySnapshots:     true,
		SupportsLinkedClones:        true,
		SupportsImageImport:         true,
		SupportedDiskTypes:          []string{"qcow2", "vmdk"},
		SupportedNetworkTypes:       []string{"virtio", "e1000"},
		SupportsDiskExport:          true,
		SupportsDiskImport:          true,
		SupportedExportFormats:      []string{"qcow2"},
		SupportedImportFormats:      []string{"vmdk"},
		SupportsExportCompression:   true,
		SupportedExportBackends:     []string{"pvc"},
		SupportedImportBackends:     []string{"pvc"},
		SupportedTransferModes:      []string{"relay"},
	}

	got := capabilitiesToReported(caps)
	require.NotNil(t, got)

	assert.True(t, got.SupportsReconfigureOnline)
	assert.True(t, got.SupportsDiskExpansionOnline)
	assert.True(t, got.SupportsSnapshots)
	assert.True(t, got.SupportsMemorySnapshots)
	assert.True(t, got.SupportsLinkedClones)
	assert.True(t, got.SupportsImageImport)
	assert.Equal(t, []string{"qcow2", "vmdk"}, got.SupportedDiskTypes)
	assert.Equal(t, []string{"virtio", "e1000"}, got.SupportedNetworkTypes)
	assert.True(t, got.SupportsDiskExport)
	assert.True(t, got.SupportsDiskImport)
	assert.Equal(t, []string{"qcow2"}, got.SupportedExportFormats)
	assert.Equal(t, []string{"vmdk"}, got.SupportedImportFormats)
	assert.True(t, got.SupportsExportCompression)
	// ADR-0006 Slice 0: backend/transfer-mode sets surface field-for-field.
	assert.Equal(t, []string{"pvc"}, got.SupportedExportBackends)
	assert.Equal(t, []string{"pvc"}, got.SupportedImportBackends)
	assert.Equal(t, []string{"relay"}, got.SupportedTransferModes)
}
