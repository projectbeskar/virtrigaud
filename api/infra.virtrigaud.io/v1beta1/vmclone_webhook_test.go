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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cloneWithSecrets is a VMClone whose spec.customization sets every unused
// secret field, and the Secret references next to them.
func cloneWithSecrets() *VMClone {
	return &VMClone{Spec: VMCloneSpec{Customization: &VMCustomization{
		Hostname: "web-01",
		Sysprep: &SysprepCustomization{
			ProductKey:    "PRODUCT-KEY-XXXXX",
			AdminPassword: &PasswordSpec{Value: "ADMIN-PW-s3cr3t", SecretRef: &LocalObjectReference{Name: "admin"}},
			JoinDomain:    &DomainJoinSpec{Domain: "corp.example.com", Username: "joiner", Password: PasswordSpec{Value: "JOIN-PW-s3cr3t"}},
		},
		Certificates: []CertificateSpec{
			{Name: "by-ref", SecretRef: &LocalObjectReference{Name: "ca"}},
			{Name: "inline", Data: "-----BEGIN PRIVATE KEY-----CERTKEY"},
		},
	}}}
}

func TestVMCustomization_UnusedSecretFields(t *testing.T) {
	assert.Equal(t, []string{
		"spec.customization.sysprep.productKey",
		"spec.customization.sysprep.adminPassword.value",
		"spec.customization.sysprep.joinDomain.password.value",
		"spec.customization.certificates[1].data",
	}, cloneWithSecrets().Spec.Customization.UnusedSecretFields())

	// Secret references, and no customization at all, are not warned about.
	byRef := &VMCustomization{
		Sysprep: &SysprepCustomization{
			AdminPassword: &PasswordSpec{SecretRef: &LocalObjectReference{Name: "admin"}, SecretKey: "password"},
			JoinDomain:    &DomainJoinSpec{Domain: "corp.example.com", Password: PasswordSpec{SecretRef: &LocalObjectReference{Name: "join"}}},
		},
		Certificates: []CertificateSpec{{Name: "ca", SecretRef: &LocalObjectReference{Name: "ca"}}},
	}
	assert.Empty(t, byRef.UnusedSecretFields())
	var none *VMCustomization
	assert.Empty(t, none.UnusedSecretFields())
}

// TestVMCloneCustomizationValidator_WarnsNeverRefuses: one warning per unused
// secret field on create and update, naming the field and never its value;
// nothing is ever refused.
func TestVMCloneCustomizationValidator_WarnsNeverRefuses(t *testing.T) {
	v := &VMCloneCustomizationValidator{}
	ctx := context.Background()

	warnings, err := v.ValidateCreate(ctx, cloneWithSecrets())
	require.NoError(t, err)
	require.Len(t, warnings, 4)
	assert.Equal(t, "spec.customization.sysprep.adminPassword.value: accepted but not acted on in this release; "+
		"it is stored in plaintext in the object — do not put a real password, product key or certificate key here", warnings[1])
	all := strings.Join(warnings, "\n")
	for _, secret := range []string{"PRODUCT-KEY-XXXXX", "ADMIN-PW-s3cr3t", "JOIN-PW-s3cr3t", "CERTKEY"} {
		assert.NotContains(t, all, secret)
	}

	warnings, err = v.ValidateUpdate(ctx, &VMClone{}, cloneWithSecrets())
	require.NoError(t, err)
	assert.Len(t, warnings, 4)

	warnings, err = v.ValidateCreate(ctx, &VMClone{})
	require.NoError(t, err)
	assert.Empty(t, warnings)

	warnings, err = v.ValidateDelete(ctx, cloneWithSecrets())
	require.NoError(t, err)
	assert.Empty(t, warnings)

	_, err = v.ValidateCreate(ctx, &VirtualMachine{})
	assert.Error(t, err)
	_, err = v.ValidateUpdate(ctx, &VMClone{}, &VirtualMachine{})
	assert.Error(t, err)
}
