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

package v1beta1

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatedValidator is a ProviderCustomValidator whose development gate enables
// cloudhypervisor, as a virtrigaud_dev manager started with
// --enable-dev-provider-types=cloudhypervisor builds it.
func gatedValidator() *ProviderCustomValidator {
	return &ProviderCustomValidator{
		EnabledDevProviderTypes: map[ProviderType]struct{}{ProviderTypeCloudHypervisor: {}},
	}
}

// capturingContext returns a context carrying a logger that appends every
// log line to *lines.
func capturingContext(lines *[]string) context.Context {
	logger := funcr.New(func(prefix, args string) {
		*lines = append(*lines, prefix+" "+args)
	}, funcr.Options{})
	return logr.NewContext(context.Background(), logger)
}

// assertInvalidOn asserts err is an apierrors Invalid error with a cause on
// fieldPath whose message contains want.
func assertInvalidOn(t *testing.T, err error, fieldPath, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an invalid-field error on %s, got nil", fieldPath)
	}
	statusErr, ok := err.(*apierrors.StatusError)
	if !ok || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an apierrors Invalid error, got %T: %v", err, err)
	}
	for _, cause := range statusErr.Status().Details.Causes {
		if cause.Field == fieldPath && cause.Type == metav1.CauseTypeFieldValueInvalid {
			if !strings.Contains(cause.Message, want) {
				t.Fatalf("cause on %s = %q, want it to contain %q", fieldPath, cause.Message, want)
			}
			return
		}
	}
	t.Fatalf("expected a FieldValueInvalid cause on %s, got %+v", fieldPath, statusErr.Status().Details.Causes)
}

func TestIsDevOnlyProviderType(t *testing.T) {
	if !IsDevOnlyProviderType(ProviderTypeCloudHypervisor) {
		t.Error("cloudhypervisor must be development-only")
	}
	for _, pt := range []ProviderType{ProviderTypeVSphere, ProviderTypeLibvirt, ProviderTypeProxmox,
		ProviderTypeFirecracker, ProviderTypeQEMU, ""} {
		if IsDevOnlyProviderType(pt) {
			t.Errorf("%q must not be development-only", pt)
		}
	}
}

// Every type that requires topology=cluster must also be allowed it.
func TestTopologyClusterRequiredTypesAreAllowed(t *testing.T) {
	for pt := range topologyClusterRequiredTypes {
		if _, ok := topologyClusterAllowedTypes[pt]; !ok {
			t.Errorf("%q requires topology cluster but is not in topologyClusterAllowedTypes", pt)
		}
	}
}

func TestProviderWebhook_CloudHypervisorWithoutGateIsRejected(t *testing.T) {
	for _, v := range []*ProviderCustomValidator{
		{},
		// A gate that enables nothing (the flag unset in a development build).
		{EnabledDevProviderTypes: map[ProviderType]struct{}{}},
	} {
		for _, topology := range []string{ProviderTopologyCluster, ProviderTopologySingle, ""} {
			warnings, err := v.ValidateCreate(context.Background(),
				newProviderForTopology("ch", ProviderTypeCloudHypervisor, topology))
			assertInvalidOn(t, err, "spec.type", "not available in this build")
			assertInvalidOn(t, err, "spec.type", "--"+DevProviderTypesFlagName+"=cloudhypervisor")
			if len(warnings) != 0 {
				t.Errorf("topology %q: expected no warnings on a refusal, got %v", topology, warnings)
			}
		}
	}
}

func TestProviderWebhook_CloudHypervisorWithGateAndClusterIsAllowedWithWarning(t *testing.T) {
	var lines []string
	ctx := capturingContext(&lines)
	v := gatedValidator()

	p := newProviderForTopology("ch", ProviderTypeCloudHypervisor, ProviderTopologyCluster)
	p.Namespace = "ns"
	warnings, err := v.ValidateCreate(ctx, p)
	if err != nil {
		t.Fatalf("expected the gated create to be admitted, got %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "development-only") ||
		!strings.Contains(warnings[0], "not available in release builds") {
		t.Fatalf("expected one development-only warning, got %v", warnings)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "development-only provider type") ||
		!strings.Contains(lines[0], `"ns/ch"`) || !strings.Contains(lines[0], `"create"`) {
		t.Fatalf("expected one admission log line naming the Provider, got %v", lines)
	}

	// Every admission warns and logs: an update too, with or without a spec
	// change.
	updated := p.DeepCopy()
	updated.Labels = map[string]string{"a": "b"}
	warnings, err = v.ValidateUpdate(ctx, p, updated)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("metadata update: expected admitted with one warning, got warnings=%v err=%v", warnings, err)
	}
	specChange := p.DeepCopy()
	specChange.Spec.Endpoint = "https://changed.example.com"
	warnings, err = v.ValidateUpdate(ctx, p, specChange)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("spec update: expected admitted with one warning, got warnings=%v err=%v", warnings, err)
	}
	if len(lines) != 3 || !strings.Contains(lines[2], `"update"`) {
		t.Fatalf("expected a log line on every admission, got %v", lines)
	}
}

func TestProviderWebhook_CloudHypervisorWithGateRequiresCluster(t *testing.T) {
	v := gatedValidator()
	for _, topology := range []string{ProviderTopologySingle, ""} {
		_, err := v.ValidateCreate(context.Background(),
			newProviderForTopology("ch", ProviderTypeCloudHypervisor, topology))
		assertInvalidOn(t, err, "spec.topology", `requires topology "cluster"`)
	}
}

func TestProviderWebhook_GateDoesNotChangeOtherTypes(t *testing.T) {
	v := gatedValidator()
	if w, err := v.ValidateCreate(context.Background(),
		newProviderForTopology("lv", ProviderTypeLibvirt, ProviderTopologyCluster)); err != nil || len(w) != 0 {
		t.Fatalf("libvirt cluster: expected admitted without warnings, got warnings=%v err=%v", w, err)
	}
	_, err := v.ValidateCreate(context.Background(),
		newProviderForTopology("vs", ProviderTypeVSphere, ProviderTopologyCluster))
	assertTopologyResult(t, err, true)
}

func TestProviderWebhook_CloudHypervisorUpdateWithoutGate(t *testing.T) {
	v := &ProviderCustomValidator{}
	old := newProviderForTopology("ch", ProviderTypeCloudHypervisor, ProviderTopologyCluster)

	// A Provider created while the gate was on can still take and lose its
	// finalizers (and so be deleted) once it is off.
	meta := old.DeepCopy()
	meta.Finalizers = []string{ProviderInUseFinalizer}
	warnings, err := v.ValidateUpdate(context.Background(), old, meta)
	if err != nil {
		t.Fatalf("metadata-only update must be admitted, got %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not available in this build") {
		t.Fatalf("expected one not-available warning, got %v", warnings)
	}

	// A spec change is refused.
	spec := old.DeepCopy()
	spec.Spec.Endpoint = "https://changed.example.com"
	_, err = v.ValidateUpdate(context.Background(), old, spec)
	assertInvalidOn(t, err, "spec.type", "not available in this build")
}

func TestProviderWebhook_CloudHypervisorImmutabilityHoldsWithGate(t *testing.T) {
	v := gatedValidator()
	old := newProviderForTopology("ch", ProviderTypeCloudHypervisor, ProviderTopologyCluster)

	retyped := old.DeepCopy()
	retyped.Spec.Type = ProviderTypeLibvirt
	_, err := v.ValidateUpdate(context.Background(), old, retyped)
	assertInvalidOn(t, err, "spec.type", "immutable")

	toCH := newProviderForTopology("lv", ProviderTypeLibvirt, ProviderTopologyCluster)
	toCHNew := toCH.DeepCopy()
	toCHNew.Spec.Type = ProviderTypeCloudHypervisor
	_, err = v.ValidateUpdate(context.Background(), toCH, toCHNew)
	assertInvalidOn(t, err, "spec.type", "immutable")

	single := old.DeepCopy()
	single.Spec.Topology = ProviderTopologySingle
	_, err = v.ValidateUpdate(context.Background(), old, single)
	assertInvalidOn(t, err, "spec.topology", "immutable")
}
