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
	"bytes"
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestBuildCreateRequest_ImageURLLoggedRedacted: the create request's debug
// log names a libvirt VMImage's source URL without its user-info or query;
// the request itself carries the URL as written (the provider needs it).
func TestBuildCreateRequest_ImageURLLoggedRedacted(t *testing.T) {
	const url = "https://joe:p4ssw0rd@images.example.com/ubuntu.qcow2?X-Amz-Signature=supersecret"
	s := cloudInitScheme(t)
	r := &VirtualMachineReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "p"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "c"},
		},
	}
	vmClass := &infrav1beta1.VMClass{Spec: infrav1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")}}
	vmImage := &infrav1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default"},
		Spec: infrav1beta1.VMImageSpec{Source: infrav1beta1.ImageSource{
			Libvirt: &infrav1beta1.LibvirtImageSource{URL: url},
		}},
	}

	var logs bytes.Buffer
	sink := funcr.New(func(prefix, args string) { logs.WriteString(prefix + args + "\n") }, funcr.Options{Verbosity: 10})
	ctx := ctrllog.IntoContext(context.Background(), logr.New(sink.GetSink()))
	req, err := r.buildCreateRequest(ctx, vm, nil, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.Equal(t, url, req.Image.URL)

	require.Contains(t, logs.String(), "Libvirt image source found")
	assert.Contains(t, logs.String(), "https://images.example.com/ubuntu.qcow2?…")
	for _, secret := range []string{"joe", "p4ssw0rd", "supersecret"} {
		assert.NotContains(t, logs.String(), secret)
	}
}
