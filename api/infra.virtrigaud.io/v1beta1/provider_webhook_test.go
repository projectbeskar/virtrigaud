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

package v1beta1

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newProviderForTopology builds a minimal Provider with only the fields the
// topology webhook inspects (type + topology). Other required spec fields are
// intentionally omitted — the CustomValidator only reads spec.type/spec.topology.
func newProviderForTopology(name string, ptype ProviderType, topology string) *Provider {
	return &Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ProviderSpec{
			Type:     ptype,
			Topology: topology,
		},
	}
}

// assertTopologyResult asserts that err is nil when wantErr is false, and that
// when wantErr is true err is an apierrors Invalid error carrying a
// FieldValueInvalid cause on spec.topology for kind Provider (not a bare error).
func assertTopologyResult(t *testing.T, err error, wantErr bool) {
	t.Helper()
	if !wantErr {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("expected an invalid-field error, got nil")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an apierrors Invalid error, got %T: %v", err, err)
	}
	statusErr, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("expected *apierrors.StatusError, got %T", err)
	}
	details := statusErr.Status().Details
	if details == nil {
		t.Fatalf("expected status details on the invalid error, got none")
	}
	if details.Kind != "Provider" {
		t.Errorf("expected invalid error Kind=Provider, got %q", details.Kind)
	}
	found := false
	for _, cause := range details.Causes {
		if cause.Field == "spec.topology" && cause.Type == metav1.CauseTypeFieldValueInvalid {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a FieldValueInvalid cause on spec.topology, got causes %+v", details.Causes)
	}
}

func TestProviderCustomValidator_ValidateCreate(t *testing.T) {
	v := &ProviderCustomValidator{}
	ctx := context.Background()

	tests := []struct {
		name     string
		ptype    ProviderType
		topology string
		wantErr  bool
	}{
		// libvirt is the only allow-listed clustered type today (ADR-0007 D2).
		{"libvirt + cluster is allowed", ProviderTypeLibvirt, ProviderTopologyCluster, false},
		{"libvirt + single is allowed", ProviderTypeLibvirt, ProviderTopologySingle, false},
		{"libvirt + empty (defaults single) is allowed", ProviderTypeLibvirt, "", false},

		// Every non-libvirt type must reject cluster.
		{"vsphere + cluster is rejected", ProviderTypeVSphere, ProviderTopologyCluster, true},
		{"proxmox + cluster is rejected", ProviderTypeProxmox, ProviderTopologyCluster, true},
		{"firecracker + cluster is rejected", ProviderTypeFirecracker, ProviderTopologyCluster, true},
		{"qemu + cluster is rejected", ProviderTypeQEMU, ProviderTopologyCluster, true},

		// Non-libvirt types are untouched for single/empty — today's providers
		// must be byte-for-byte unaffected (ADR-0007 D9).
		{"vsphere + single is allowed", ProviderTypeVSphere, ProviderTopologySingle, false},
		{"vsphere + empty is allowed", ProviderTypeVSphere, "", false},
		{"proxmox + single is allowed", ProviderTypeProxmox, ProviderTopologySingle, false},
		{"proxmox + empty is allowed", ProviderTypeProxmox, "", false},
		{"firecracker + single is allowed", ProviderTypeFirecracker, ProviderTopologySingle, false},
		{"qemu + empty is allowed", ProviderTypeQEMU, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := v.ValidateCreate(ctx, newProviderForTopology("p", tc.ptype, tc.topology))
			if warnings != nil {
				t.Errorf("expected nil admission warnings, got %v", warnings)
			}
			assertTopologyResult(t, err, tc.wantErr)
		})
	}
}

func TestProviderCustomValidator_ValidateUpdate(t *testing.T) {
	v := &ProviderCustomValidator{}
	ctx := context.Background()

	tests := []struct {
		name                     string
		oldType, newType         ProviderType
		oldTopology, newTopology string
		wantErr                  bool
	}{
		// A patch that flips an existing non-libvirt provider into cluster must
		// be rejected (the core ValidateUpdate requirement, ADR-0007 D2).
		{"single -> cluster on vsphere is rejected", ProviderTypeVSphere, ProviderTypeVSphere, ProviderTopologySingle, ProviderTopologyCluster, true},
		{"single -> cluster on proxmox is rejected", ProviderTypeProxmox, ProviderTypeProxmox, ProviderTopologySingle, ProviderTopologyCluster, true},

		// A cluster libvirt provider may be updated while staying clustered.
		{"cluster -> cluster on libvirt is allowed", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologyCluster, ProviderTopologyCluster, false},
		{"single -> single on libvirt is allowed", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologySingle, ProviderTopologySingle, false},

		// Topology is immutable (ADR-0007 Addendum A): VMs are placed and their
		// per-VM calls routed/owner-checked under one topology, so any flip is
		// rejected — including the former "self-heal" cluster -> single on a
		// non-libvirt type (such an object must be recreated).
		{"single -> cluster on libvirt is rejected (immutable)", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologySingle, ProviderTopologyCluster, true},
		{"cluster -> single on libvirt is rejected (immutable)", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologyCluster, ProviderTopologySingle, true},
		{"cluster -> single on vsphere is rejected (immutable)", ProviderTypeVSphere, ProviderTypeVSphere, ProviderTopologyCluster, ProviderTopologySingle, true},

		// "" is the defaulted "single": a Provider created before the field
		// existed (or a client that omits it) keeps accepting ordinary updates,
		// but "" <-> cluster is still a flip.
		{"unset -> single is allowed", ProviderTypeLibvirt, ProviderTypeLibvirt, "", ProviderTopologySingle, false},
		{"single -> unset is allowed", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologySingle, "", false},
		{"unset -> unset is allowed", ProviderTypeVSphere, ProviderTypeVSphere, "", "", false},
		{"unset -> cluster is rejected (immutable)", ProviderTypeLibvirt, ProviderTypeLibvirt, "", ProviderTopologyCluster, true},
		{"cluster -> unset is rejected (immutable)", ProviderTypeLibvirt, ProviderTypeLibvirt, ProviderTopologyCluster, "", true},

		// Flipping the type out of libvirt while remaining cluster is rejected
		// (the D2 rule is evaluated on the new object's type+topology).
		{"type libvirt -> vsphere while cluster is rejected", ProviderTypeLibvirt, ProviderTypeVSphere, ProviderTopologyCluster, ProviderTopologyCluster, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldObj := newProviderForTopology("p", tc.oldType, tc.oldTopology)
			newObj := newProviderForTopology("p", tc.newType, tc.newTopology)
			warnings, err := v.ValidateUpdate(ctx, oldObj, newObj)
			if warnings != nil {
				t.Errorf("expected nil admission warnings, got %v", warnings)
			}
			assertTopologyResult(t, err, tc.wantErr)
		})
	}
}

func TestProviderCustomValidator_ValidateDelete_IsNoOp(t *testing.T) {
	v := &ProviderCustomValidator{}
	// Even a Provider that would fail create/update validation must delete
	// cleanly — deletion is always allowed.
	warnings, err := v.ValidateDelete(context.Background(), newProviderForTopology("p", ProviderTypeVSphere, ProviderTopologyCluster))
	if err != nil {
		t.Fatalf("expected ValidateDelete to be a no-op, got err %v", err)
	}
	if warnings != nil {
		t.Fatalf("expected ValidateDelete to return nil warnings, got %v", warnings)
	}
}

// TestProviderCustomValidator_Message pins that the rejection message is
// actionable: it names the field's value (cluster), the offending type, and the
// allowed type(s), so an operator can self-correct without reading source.
func TestProviderCustomValidator_Message(t *testing.T) {
	v := &ProviderCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), newProviderForTopology("myprov", ProviderTypeVSphere, ProviderTopologyCluster))
	if err == nil {
		t.Fatal("expected an error for vsphere + cluster")
	}
	msg := err.Error()
	for _, want := range []string{"topology", ProviderTopologyCluster, string(ProviderTypeVSphere), string(ProviderTypeLibvirt)} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection message %q does not mention %q", msg, want)
		}
	}
}

// TestProviderCustomValidator_WrongType ensures a non-Provider object yields a
// plain (non-Invalid) error rather than panicking or misreporting a field error.
func TestProviderCustomValidator_WrongType(t *testing.T) {
	v := &ProviderCustomValidator{}
	ctx := context.Background()

	if _, err := v.ValidateCreate(ctx, &ProviderList{}); err == nil {
		t.Error("expected ValidateCreate to error on a non-Provider object")
	} else if apierrors.IsInvalid(err) {
		t.Errorf("wrong-type error should not be an Invalid field error, got %v", err)
	}

	if _, err := v.ValidateUpdate(ctx, &ProviderList{}, &ProviderList{}); err == nil {
		t.Error("expected ValidateUpdate to error on a non-Provider object")
	} else if apierrors.IsInvalid(err) {
		t.Errorf("wrong-type error should not be an Invalid field error, got %v", err)
	}
}
