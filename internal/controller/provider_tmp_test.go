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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These tests pin the sizeLimit of the provider pod's writable /tmp emptyDir:
// the manager-wide default (--provider-tmp-size-limit), the per-Provider
// annotation override, "none", and that the memory-backed SSH-key tmpfs keeps
// its own small limit.

// reconciledProviderVolumes reconciles a plaintext Provider with the given
// annotations through a reconciler with tmpLimit, and returns its Deployment's
// pod volumes by name.
func reconciledProviderVolumes(t *testing.T, providerType infravirtrigaudiov1beta1.ProviderType, tmpLimit string, annotations map[string]string) map[string]corev1.Volume {
	t.Helper()
	sch := newProviderSAScheme(t)
	prov := providerWithRuntime("tmp-limit", &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	prov.Spec.Type = providerType
	prov.Annotations = annotations
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch, TmpSizeLimit: tmpLimit}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "tmp-limit", Namespace: "default"},
	})
	require.NoError(t, err)
	dep := &appsv1.Deployment{}
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Name: "virtrigaud-provider-default-tmp-limit", Namespace: "default"}, dep))
	out := map[string]corev1.Volume{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		out[v.Name] = v
	}
	return out
}

// tmpSizeLimitOf returns the sizeLimit of the "tmp" volume ("" for none).
func tmpSizeLimitOf(t *testing.T, vols map[string]corev1.Volume) string {
	t.Helper()
	tmp, ok := vols["tmp"]
	require.True(t, ok, "the provider pod has a writable /tmp volume")
	require.NotNil(t, tmp.EmptyDir)
	assert.Empty(t, tmp.EmptyDir.Medium, "/tmp stays disk-backed (staging must not consume memory)")
	if tmp.EmptyDir.SizeLimit == nil {
		return ""
	}
	return tmp.EmptyDir.SizeLimit.String()
}

func TestProviderTmp_PerTypeDefaults(t *testing.T) {
	cases := map[infravirtrigaudiov1beta1.ProviderType]string{
		infravirtrigaudiov1beta1.ProviderTypeVSphere: "512Gi", // stages whole disks and OVAs in /tmp
		infravirtrigaudiov1beta1.ProviderTypeLibvirt: "1Gi",   // streams over SSH; KB of SSH material
		infravirtrigaudiov1beta1.ProviderTypeProxmox: "1Gi",
	}
	for providerType, want := range cases {
		t.Run(string(providerType), func(t *testing.T) {
			assert.Equal(t, want, tmpSizeLimitOf(t, reconciledProviderVolumes(t, providerType, "", nil)))
		})
	}
}

func TestProviderTmp_LibvirtSSHKeyTmpfsKeepsItsOwnLimit(t *testing.T) {
	vols := reconciledProviderVolumes(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, "", nil)
	key := vols[sshKeyVolumeName]
	require.NotNil(t, key.EmptyDir)
	assert.Equal(t, corev1.StorageMediumMemory, key.EmptyDir.Medium)
	assert.Equal(t, "4Mi", key.EmptyDir.SizeLimit.String())
}

func TestProviderTmp_Precedence(t *testing.T) {
	vsphere := infravirtrigaudiov1beta1.ProviderTypeVSphere
	cases := map[string]struct {
		manager    string
		annotation *string
		want       string
	}{
		"the manager-wide limit replaces the per-type default": {manager: "20Gi", want: "20Gi"},
		"the manager-wide none removes the limit":              {manager: infravirtrigaudiov1beta1.ProviderTmpSizeLimitNone, want: ""},
		"the annotation beats the manager-wide limit":          {manager: "20Gi", annotation: ptrTo("50Gi"), want: "50Gi"},
		"the annotation none removes the limit":                {annotation: ptrTo(infravirtrigaudiov1beta1.ProviderTmpSizeLimitNone), want: ""},
		"an invalid annotation is ignored":                     {manager: "20Gi", annotation: ptrTo("lots"), want: "20Gi"},
		"a non-positive annotation is ignored":                 {annotation: ptrTo("0"), want: "512Gi"},
		"an invalid manager-wide limit is ignored":             {manager: "lots", want: "512Gi"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ann map[string]string
			if tc.annotation != nil {
				ann = map[string]string{infravirtrigaudiov1beta1.ProviderTmpSizeLimitAnnotation: *tc.annotation}
			}
			assert.Equal(t, tc.want, tmpSizeLimitOf(t, reconciledProviderVolumes(t, vsphere, tc.manager, ann)))
		})
	}
}

// ptrTo returns a pointer to v.
func ptrTo(v string) *string { return &v }
