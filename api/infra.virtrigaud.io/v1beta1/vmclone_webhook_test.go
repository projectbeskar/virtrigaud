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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cloneWithSecrets is a VMClone whose spec.customization sets every unused
// secret field, and the Secret references next to them.
func cloneWithSecrets() *VMClone {
	return &VMClone{Spec: VMCloneSpec{Customization: &VMCustomization{
		Hostname: "web-01",
		Sysprep: &SysprepCustomization{
			ProductKey:     "PRODUCT-KEY-XXXXX",
			AdminPassword:  &PasswordSpec{Value: "ADMIN-PW-s3cr3t", SecretRef: &LocalObjectReference{Name: "admin"}},
			JoinDomain:     &DomainJoinSpec{Domain: "corp.example.com", Username: "joiner", Password: PasswordSpec{Value: "JOIN-PW-s3cr3t"}},
			CustomCommands: []string{"net user Administrator CMD-PW-s3cr3t"},
		},
		Certificates: []CertificateSpec{
			{Name: "by-ref", SecretRef: &LocalObjectReference{Name: "ca"}},
			{Name: "inline", Data: "-----BEGIN PRIVATE KEY-----CERTKEY"},
		},
		GuestCommands: []GuestCommand{
			{Command: "/usr/bin/enroll --token GUEST-TOKEN", Arguments: []string{"GUEST-ARG"}},
			{Command: "/bin/true"},
		},
		UserData: &UserData{Ignition: &Ignition{Inline: `{"passwd":"IGN-s3cr3t"}`}},
	}}}
}

func TestVMCustomization_UnusedSecretFields(t *testing.T) {
	assert.Equal(t, []string{
		"spec.customization.sysprep.productKey",
		"spec.customization.sysprep.adminPassword.value",
		"spec.customization.sysprep.joinDomain.password.value",
		"spec.customization.sysprep.customCommands",
		"spec.customization.certificates[1].data",
		"spec.customization.guestCommands[0].command",
		"spec.customization.guestCommands[0].arguments",
		"spec.customization.guestCommands[1].command",
		"spec.customization.userData.ignition.inline",
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

// TestVMCloneCustomizationValidator_WarnsNeverRefuses: one warning naming
// the unused secret fields on create and on an update that changes
// spec.customization, never their values; nothing is ever refused.
func TestVMCloneCustomizationValidator_WarnsNeverRefuses(t *testing.T) {
	v := &VMCloneCustomizationValidator{}
	ctx := context.Background()

	warnings, err := v.ValidateCreate(ctx, cloneWithSecrets())
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Equal(t, "spec.customization.sysprep.productKey, spec.customization.sysprep.adminPassword.value, "+
		"spec.customization.sysprep.joinDomain.password.value, spec.customization.sysprep.customCommands, "+
		"spec.customization.certificates[1].data, spec.customization.guestCommands[0].command, "+
		"spec.customization.guestCommands[0].arguments, spec.customization.guestCommands[1].command (+1 more): "+
		"accepted but not acted on in this release; stored in plaintext in the object — "+
		"do not put a real password, key or other credential here", warnings[0])
	for _, secret := range []string{"PRODUCT-KEY-XXXXX", "ADMIN-PW-s3cr3t", "JOIN-PW-s3cr3t", "CERTKEY",
		"CMD-PW-s3cr3t", "GUEST-TOKEN", "GUEST-ARG", "IGN-s3cr3t"} {
		assert.NotContains(t, warnings[0], secret)
	}

	// An update that sets the customization warns.
	warnings, err = v.ValidateUpdate(ctx, &VMClone{}, cloneWithSecrets())
	require.NoError(t, err)
	assert.Len(t, warnings, 1)

	// An update that leaves it as it was (a finalizer write), or of a VMClone
	// being deleted, does not.
	withFinalizer := cloneWithSecrets()
	withFinalizer.Finalizers = []string{"infra.virtrigaud.io/finalizer"}
	warnings, err = v.ValidateUpdate(ctx, cloneWithSecrets(), withFinalizer)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	deleting := cloneWithSecrets()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Spec.Customization.Hostname = "changed"
	warnings, err = v.ValidateUpdate(ctx, cloneWithSecrets(), deleting)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	// Few fields are all named, without a count.
	few := &VMClone{Spec: VMCloneSpec{Customization: &VMCustomization{
		Sysprep: &SysprepCustomization{ProductKey: "PRODUCT-KEY-XXXXX"}}}}
	warnings, err = v.ValidateCreate(ctx, few)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.True(t, strings.HasPrefix(warnings[0], "spec.customization.sysprep.productKey: accepted"), warnings[0])

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
	_, err = v.ValidateUpdate(ctx, &VirtualMachine{}, &VMClone{})
	assert.Error(t, err)
}

// TestCustomizationWarnings_StayWithinTheBudget: even the most fields the CRD
// admits give one warning far below the API server's 4096-character budget.
func TestCustomizationWarnings_StayWithinTheBudget(t *testing.T) {
	c := &VMCustomization{Sysprep: &SysprepCustomization{ProductKey: "k", AdminPassword: &PasswordSpec{Value: "p"},
		JoinDomain: &DomainJoinSpec{Password: PasswordSpec{Value: "p"}}, CustomCommands: []string{"c"}},
		UserData: &UserData{Ignition: &Ignition{Inline: "i"}}}
	for i := 0; i < 10; i++ {
		c.Certificates = append(c.Certificates, CertificateSpec{Data: "d"})
	}
	for i := 0; i < 20; i++ {
		c.GuestCommands = append(c.GuestCommands, GuestCommand{Command: "c", Arguments: []string{"a"}})
	}
	warnings := customizationWarnings(&VMClone{Spec: VMCloneSpec{Customization: c}})
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "(+47 more)")
	assert.Less(t, len(warnings[0]), 1024)
}
