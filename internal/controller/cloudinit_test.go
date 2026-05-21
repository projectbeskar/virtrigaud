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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func cloudInitScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := infravirtrigaudiov1beta1.AddToScheme(s); err != nil {
		t.Fatalf("add virtrigaud scheme: %v", err)
	}
	return s
}

func reconcilerWithSecrets(t *testing.T, secrets ...*corev1.Secret) *VirtualMachineReconciler {
	t.Helper()
	s := cloudInitScheme(t)
	objs := make([]client.Object, len(secrets))
	for i, sec := range secrets {
		objs[i] = sec
	}
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &VirtualMachineReconciler{Client: fc, Scheme: s}
}

func makeSecret(name, ns string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
}

// ─── extractCloudInitFromSecret ───────────────────────────────────────────────

func TestExtractCloudInitFromSecret_UserdataKey(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{"userdata": []byte("#cloud-config\npackages: [git]")})
	got, err := extractCloudInitFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "#cloud-config\npackages: [git]" {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractCloudInitFromSecret_UserDashDataKey(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{"user-data": []byte("#cloud-config\nhostname: foo")})
	got, err := extractCloudInitFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "hostname: foo") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractCloudInitFromSecret_CloudInitKey(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{"cloud-init": []byte("#cloud-config\nruncmd: [echo hi]")})
	got, err := extractCloudInitFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "runcmd") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractCloudInitFromSecret_CloudConfigKey(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{"cloud-config": []byte("#cloud-config\ntimezone: UTC")})
	got, err := extractCloudInitFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "timezone") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractCloudInitFromSecret_NoRecognisedKey_ReturnsError(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{"random-key": []byte("data")})
	_, err := extractCloudInitFromSecret(s)
	if err == nil {
		t.Fatal("expected error for unrecognised key, got nil")
	}
	if !strings.Contains(err.Error(), "accepted keys") {
		t.Errorf("error should mention accepted keys, got: %v", err)
	}
}

func TestExtractCloudInitFromSecret_EmptySecret_ReturnsError(t *testing.T) {
	s := makeSecret("ci", "default", map[string][]byte{})
	_, err := extractCloudInitFromSecret(s)
	if err == nil {
		t.Fatal("expected error for empty secret, got nil")
	}
}

// ─── extractMetaDataFromSecret ────────────────────────────────────────────────

func TestExtractMetaDataFromSecret_MetadataKey(t *testing.T) {
	s := makeSecret("md", "default", map[string][]byte{"metadata": []byte("instance-id: vm-001")})
	got, err := extractMetaDataFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "vm-001") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractMetaDataFromSecret_MetaDashDataKey(t *testing.T) {
	s := makeSecret("md", "default", map[string][]byte{"meta-data": []byte("instance-id: vm-002")})
	got, err := extractMetaDataFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "vm-002") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractMetaDataFromSecret_MetaUnderscoreDataKey(t *testing.T) {
	s := makeSecret("md", "default", map[string][]byte{"meta_data": []byte("instance-id: vm-003")})
	got, err := extractMetaDataFromSecret(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "vm-003") {
		t.Errorf("unexpected data: %q", got)
	}
}

func TestExtractMetaDataFromSecret_NoRecognisedKey_ReturnsError(t *testing.T) {
	s := makeSecret("md", "default", map[string][]byte{"random": []byte("data")})
	_, err := extractMetaDataFromSecret(s)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "accepted keys") {
		t.Errorf("error should mention accepted keys, got: %v", err)
	}
}

// ─── mergeCloudConfigParts ────────────────────────────────────────────────────

func TestMergeCloudConfigParts_TwoParts_ProducesMIME(t *testing.T) {
	parts := []string{
		"#cloud-config\npackages:\n  - nginx",
		"#cloud-config\nruncmd:\n  - echo hello",
	}
	result := mergeCloudConfigParts(parts)

	if !strings.Contains(result, "Content-Type: multipart/mixed") {
		t.Error("expected multipart/mixed Content-Type header")
	}
	if !strings.Contains(result, "MIME-Version: 1.0") {
		t.Error("expected MIME-Version header")
	}
	if !strings.Contains(result, "Content-Type: text/cloud-config") {
		t.Error("expected text/cloud-config part header")
	}
	if !strings.Contains(result, "packages") {
		t.Error("expected first part content")
	}
	if !strings.Contains(result, "runcmd") {
		t.Error("expected second part content")
	}
	if !strings.Contains(result, "VIRTRIGAUD_CLOUD_INIT_BOUNDARY--") {
		t.Error("expected closing boundary")
	}
}

func TestMergeCloudConfigParts_ThreeParts_AllPresent(t *testing.T) {
	parts := []string{"part-A", "part-B", "part-C"}
	result := mergeCloudConfigParts(parts)
	for _, p := range parts {
		if !strings.Contains(result, p) {
			t.Errorf("expected %q in merged output", p)
		}
	}
}

// ─── resolveCloudInitUserData ─────────────────────────────────────────────────

func TestResolveCloudInitUserData_InlineOnly(t *testing.T) {
	r := reconcilerWithSecrets(t)
	ci := &infravirtrigaudiov1beta1.CloudInit{Inline: "#cloud-config\nhostname: myvm"}

	got, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "#cloud-config\nhostname: myvm" {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestResolveCloudInitUserData_SecretRefOnly(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"userdata": []byte("#cloud-config\npackages:\n  - curl"),
	})
	r := reconcilerWithSecrets(t, secret)
	ci := &infravirtrigaudiov1beta1.CloudInit{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
	}

	got, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "curl") {
		t.Errorf("expected secret content in result, got: %q", got)
	}
}

func TestResolveCloudInitUserData_BothInlineAndSecretRef_ProducesMIME(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"userdata": []byte("#cloud-config\nruncmd:\n  - echo from-secret"),
	})
	r := reconcilerWithSecrets(t, secret)
	ci := &infravirtrigaudiov1beta1.CloudInit{
		Inline:    "#cloud-config\nhostname: myvm",
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
	}

	got, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "Content-Type: multipart/mixed") {
		t.Error("expected multi-part MIME when both inline and secretRef are set")
	}
	if !strings.Contains(got, "hostname: myvm") {
		t.Error("expected inline content in merged output")
	}
	if !strings.Contains(got, "from-secret") {
		t.Error("expected secretRef content in merged output")
	}
}

func TestResolveCloudInitUserData_SecretNotFound_ReturnsError(t *testing.T) {
	r := reconcilerWithSecrets(t) // no secrets registered
	ci := &infravirtrigaudiov1beta1.CloudInit{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing-secret"},
	}

	_, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "missing-secret") {
		t.Errorf("error should mention secret name, got: %v", err)
	}
}

func TestResolveCloudInitUserData_SecretHasNoRecognisedKey_ReturnsError(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"wrong-key": []byte("data"),
	})
	r := reconcilerWithSecrets(t, secret)
	ci := &infravirtrigaudiov1beta1.CloudInit{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
	}

	_, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err == nil {
		t.Fatal("expected error for unrecognised key, got nil")
	}
}

func TestResolveCloudInitUserData_NeitherInlineNorSecretRef_ReturnsEmpty(t *testing.T) {
	r := reconcilerWithSecrets(t)
	ci := &infravirtrigaudiov1beta1.CloudInit{}

	got, err := r.resolveCloudInitUserData(context.Background(), "default", ci)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got: %q", got)
	}
}

// ─── resolveCloudInitMetaData ─────────────────────────────────────────────────

func TestResolveCloudInitMetaData_InlineOnly(t *testing.T) {
	r := reconcilerWithSecrets(t)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{Inline: "instance-id: vm-001"}

	got, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "instance-id: vm-001" {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestResolveCloudInitMetaData_SecretRefOnly(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"metadata": []byte("instance-id: vm-from-secret"),
	})
	r := reconcilerWithSecrets(t, secret)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
	}

	got, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "vm-from-secret") {
		t.Errorf("expected secret content, got: %q", got)
	}
}

func TestResolveCloudInitMetaData_BothInlineAndSecretRef_Concatenated(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"metadata": []byte("local-hostname: myhost"),
	})
	r := reconcilerWithSecrets(t, secret)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{
		Inline:    "instance-id: vm-001",
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
	}

	got, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "instance-id: vm-001") {
		t.Error("expected inline content in merged output")
	}
	if !strings.Contains(got, "local-hostname: myhost") {
		t.Error("expected secretRef content in merged output")
	}
}

func TestResolveCloudInitMetaData_SecretNotFound_ReturnsError(t *testing.T) {
	r := reconcilerWithSecrets(t)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing"},
	}

	_, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should mention secret name, got: %v", err)
	}
}

func TestResolveCloudInitMetaData_SecretHasNoRecognisedKey_ReturnsError(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"bad-key": []byte("data"),
	})
	r := reconcilerWithSecrets(t, secret)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{
		SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
	}

	_, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err == nil {
		t.Fatal("expected error for unrecognised key, got nil")
	}
}

func TestResolveCloudInitMetaData_NeitherInlineNorSecretRef_ReturnsEmpty(t *testing.T) {
	r := reconcilerWithSecrets(t)
	meta := &infravirtrigaudiov1beta1.CloudInitMetaData{}

	got, err := r.resolveCloudInitMetaData(context.Background(), "default", meta)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got: %q", got)
	}
}

// ─── integration: buildCreateRequest with SecretRef ──────────────────────────

func TestBuildCreateRequest_UserData_SecretRef(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"userdata": []byte("#cloud-config\npackages:\n  - htop"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			UserData: &infravirtrigaudiov1beta1.UserData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInit{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.UserData == nil {
		t.Fatal("expected UserData to be set")
	}
	if !strings.Contains(req.UserData.CloudInitData, "htop") {
		t.Errorf("expected secret content in UserData, got: %q", req.UserData.CloudInitData)
	}
	if req.UserData.Type != "cloud-init" {
		t.Errorf("expected Type 'cloud-init', got %q", req.UserData.Type)
	}
}

func TestBuildCreateRequest_UserData_InlineAndSecretRef_Merged(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"userdata": []byte("#cloud-config\nruncmd:\n  - touch /tmp/secret"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			UserData: &infravirtrigaudiov1beta1.UserData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInit{
					Inline:    "#cloud-config\nhostname: merged-vm",
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.UserData == nil {
		t.Fatal("expected UserData to be set")
	}
	if !strings.Contains(req.UserData.CloudInitData, "multipart/mixed") {
		t.Error("expected MIME merge when both inline and secretRef are set")
	}
	if !strings.Contains(req.UserData.CloudInitData, "hostname: merged-vm") {
		t.Error("expected inline content in merged output")
	}
	if !strings.Contains(req.UserData.CloudInitData, "touch /tmp/secret") {
		t.Error("expected secretRef content in merged output")
	}
}

func TestBuildCreateRequest_UserData_SecretNotFound_ReturnsError(t *testing.T) {
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			UserData: &infravirtrigaudiov1beta1.UserData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInit{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "does-not-exist"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	_, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should mention secret name, got: %v", err)
	}
}

func TestBuildCreateRequest_MetaData_SecretRef(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"metadata": []byte("instance-id: vm-from-k8s-secret"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			MetaData: &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "vm-from-k8s-secret") {
		t.Errorf("expected secret content in MetaData, got: %q", req.MetaData.MetaDataYAML)
	}
}

func TestBuildCreateRequest_MetaData_SecretNotFound_ReturnsError(t *testing.T) {
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			MetaData: &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing-md-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	_, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err == nil {
		t.Fatal("expected error for missing MetaData secret, got nil")
	}
	if !strings.Contains(err.Error(), "missing-md-secret") {
		t.Errorf("error should mention secret name, got: %v", err)
	}
}

func TestBuildCreateRequest_UserData_EmptyCloudInit_ProducesNilUserData(t *testing.T) {
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			UserData: &infravirtrigaudiov1beta1.UserData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInit{
					// Neither Inline nor SecretRef — resolves to ""
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.UserData != nil {
		t.Errorf("expected UserData to be nil when CloudInit resolves to empty, got: %+v", req.UserData)
	}
}

func TestBuildCreateRequest_MetaData_EmptyCloudInit_ProducesNilMetaData(t *testing.T) {
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			MetaData: &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					// Neither Inline nor SecretRef — resolves to ""
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData != nil {
		t.Errorf("expected MetaData to be nil when CloudInit resolves to empty, got: %+v", req.MetaData)
	}
}

func TestBuildCreateRequest_UserData_SecretHasNoRecognisedKey_ReturnsError(t *testing.T) {
	secret := makeSecret("ci-secret", "default", map[string][]byte{
		"wrong-key": []byte("data"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			UserData: &infravirtrigaudiov1beta1.UserData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInit{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ci-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	_, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err == nil {
		t.Fatal("expected error for unrecognised secret key, got nil")
	}
	if !strings.Contains(err.Error(), "accepted keys") {
		t.Errorf("error should mention accepted keys, got: %v", err)
	}
}

func TestBuildCreateRequest_MetaData_SecretHasNoRecognisedKey_ReturnsError(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"wrong-key": []byte("data"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			MetaData: &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	_, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err == nil {
		t.Fatal("expected error for unrecognised secret key, got nil")
	}
	if !strings.Contains(err.Error(), "accepted keys") {
		t.Errorf("error should mention accepted keys, got: %v", err)
	}
}

func TestBuildCreateRequest_MetaData_InlineAndSecretRef_Concatenated(t *testing.T) {
	secret := makeSecret("md-secret", "default", map[string][]byte{
		"metadata": []byte("local-hostname: my-host"),
	})
	s := cloudInitScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VirtualMachineReconciler{Client: fc, Scheme: s}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			MetaData: &infravirtrigaudiov1beta1.MetaData{
				CloudInit: &infravirtrigaudiov1beta1.CloudInitMetaData{
					Inline:    "instance-id: vm-001",
					SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "md-secret"},
				},
			},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req, err := r.buildCreateRequest(context.Background(), vm, vmClass, nil, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MetaData == nil {
		t.Fatal("expected MetaData to be set")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "instance-id: vm-001") {
		t.Error("expected inline content in MetaData")
	}
	if !strings.Contains(req.MetaData.MetaDataYAML, "local-hostname: my-host") {
		t.Error("expected secretRef content in MetaData")
	}
}
