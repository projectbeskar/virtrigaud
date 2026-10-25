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
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/cloneidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The secret values of fullCustomization: none may leave the VMClone spec.
var customizationSecrets = []string{
	"ADMIN-PW-s3cr3t", "JOIN-PW-s3cr3t", "PRODUCT-KEY-XXXXX",
	"-----BEGIN PRIVATE KEY-----CERTKEY", "CUSTOM-CMD-pw=hunter2",
	"GUEST-CMD-token", "GUEST-ARG-password", "IGNITION-INLINE-secret",
}

// fullCustomization sets every field of spec.customization, the secret ones
// to the values of customizationSecrets.
func fullCustomization() *infrav1beta1.VMCustomization {
	timeout := metav1.Duration{Duration: 2 * time.Minute}
	return &infrav1beta1.VMCustomization{
		Hostname: "web-01",
		Domain:   "example.com",
		TimeZone: "UTC",
		Networks: []infrav1beta1.NetworkCustomization{{
			Name: "primary", IPAddress: "192.0.2.10", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1",
			DNS: []string{"192.0.2.53"}, MACAddress: "52:54:00:00:00:01", DHCP: true,
		}},
		UserData: &infrav1beta1.UserData{
			CloudInit: &infrav1beta1.CloudInit{
				Inline:    "#cloud-config\nhostname: web-01\n",
				SecretRef: &infrav1beta1.LocalObjectReference{Name: "clone-userdata"},
			},
			Ignition: &infrav1beta1.Ignition{
				Inline:    `{"ignition":{"version":"3.4.0"},"passwd":"IGNITION-INLINE-secret"}`,
				SecretRef: &infrav1beta1.LocalObjectReference{Name: "clone-ignition"},
			},
		},
		Sysprep: &infrav1beta1.SysprepCustomization{
			Enabled:      true,
			ProductKey:   "PRODUCT-KEY-XXXXX",
			Organization: "Example Org",
			Owner:        "ops",
			AdminPassword: &infrav1beta1.PasswordSpec{
				Value:     "ADMIN-PW-s3cr3t",
				SecretRef: &infrav1beta1.LocalObjectReference{Name: "admin-pw"},
				SecretKey: "password",
			},
			JoinDomain: &infrav1beta1.DomainJoinSpec{
				Domain: "corp.example.com", Username: "joiner", OrganizationalUnit: "OU=Servers",
				Password: infrav1beta1.PasswordSpec{
					Value:     "JOIN-PW-s3cr3t",
					SecretRef: &infrav1beta1.LocalObjectReference{Name: "join-pw"},
					SecretKey: "password",
				},
			},
			CustomCommands: []string{"net user Administrator CUSTOM-CMD-pw=hunter2"},
		},
		Tags: []string{"env:test"},
		GuestCommands: []infrav1beta1.GuestCommand{{
			Command:          "/usr/bin/enroll --token GUEST-CMD-token",
			Arguments:        []string{"--secret", "GUEST-ARG-password"},
			WorkingDirectory: "/opt",
			RunAsUser:        "root",
			Timeout:          &timeout,
			Stage:            "first-boot",
		}},
		Certificates: []infrav1beta1.CertificateSpec{{
			Name:      "corp-ca",
			Data:      "-----BEGIN PRIVATE KEY-----CERTKEY",
			SecretRef: &infrav1beta1.LocalObjectReference{Name: "corp-ca"},
			SecretKey: "tls.crt",
			Store:     "root",
		}},
	}
}

// assertNoCustomizationSecret fails when text contains any secret value of
// fullCustomization.
func assertNoCustomizationSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range customizationSecrets {
		assert.NotContains(t, text, s, "%s carries a secret value of spec.customization", where)
	}
}

// TestVMClone_CustomizeJSONCarriesNoUnusedSecret drives a VMClone whose
// customization sets every field through to the Clone request: the request's
// CustomizeJSON carries none of the secret values, and the providers'
// customization parser reads exactly what it read from the whole
// spec.customization (the applied fields unchanged, the same fields reported
// as not applied). No secret reaches the VMClone's status, its events or the
// manager's log, which names the fields in a Warning event instead.
func TestVMClone_CustomizeJSONCarriesNoUnusedSecret(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"
	prov := runningProvider(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-secrets", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source:        infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target:        infrav1beta1.VMCloneTarget{Name: "clone-target"},
			Customization: fullCustomization(),
		},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)
	rec := record.NewFakeRecorder(100)
	r.Recorder = rec

	var logs bytes.Buffer
	sink := funcr.New(func(prefix, args string) {
		logs.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 10})
	ctx := ctrllog.IntoContext(context.Background(), logr.New(sink.GetSink()))
	for range 4 {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
	}

	require.Equal(t, 1, cp.cloneCnt)
	sent := cp.lastClone.CustomizeJSON
	require.NotEmpty(t, sent)
	assertNoCustomizationSecret(t, "CustomizeJSON", sent)

	// The providers read the same customization from it.
	whole, err := json.Marshal(fullCustomization())
	require.NoError(t, err)
	want, err := cloneidentity.ParseCustomization(string(whole))
	require.NoError(t, err)
	got, err := cloneidentity.ParseCustomization(sent)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, "web-01", got.Hostname)
	assert.Equal(t, "example.com", got.Domain)
	assert.Equal(t, "#cloud-config\nhostname: web-01\n", got.InlineUserData)
	assert.True(t, got.SecretUserData)
	assert.Equal(t, []string{"certificates", "guestCommands", "networks", "sysprep", "tags", "timeZone", "userData.ignition"},
		got.Unapplied)

	// The VMClone's status and conditions carry no secret.
	live := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, live.Status.Phase)
	status, err := json.Marshal(live.Status)
	require.NoError(t, err)
	assertNoCustomizationSecret(t, "status", string(status))
	assert.Equal(t, fullCustomization(), live.Spec.Customization, "the spec is never rewritten")

	// One Warning event names the unused secret fields, never their values.
	close(rec.Events)
	var events []string
	for e := range rec.Events {
		events = append(events, e)
	}
	all := strings.Join(events, "\n")
	assertNoCustomizationSecret(t, "events", all)
	assert.Contains(t, all, "Warning "+customizationNotSentReason+" spec.customization.sysprep.productKey, "+
		"spec.customization.sysprep.adminPassword.value, spec.customization.sysprep.joinDomain.password.value, "+
		"spec.customization.certificates[0].data: accepted but not acted on")

	require.NotEmpty(t, logs.String(), "the logger captured the reconcile")
	assertNoCustomizationSecret(t, "the manager log", logs.String())
}

// TestVMClone_CustomizeJSONWithoutSecrets: a customization that holds no
// unused secret records no Warning event, and its applied fields are sent as
// they are.
func TestVMClone_CustomizeJSONWithoutSecrets(t *testing.T) {
	clone := &infrav1beta1.VMClone{Spec: infrav1beta1.VMCloneSpec{Customization: &infrav1beta1.VMCustomization{
		Hostname: "web-02", Domain: "example.com",
		UserData: &infrav1beta1.UserData{CloudInit: &infrav1beta1.CloudInit{Inline: "#cloud-config\n"}},
	}}}
	rec := record.NewFakeRecorder(10)
	r := &VMCloneReconciler{Recorder: rec}
	assert.JSONEq(t, `{"hostname":"web-02","domain":"example.com","userData":{"cloudInit":{"inline":"#cloud-config\n"}}}`,
		r.customizeJSON(context.Background(), clone))
	assert.Empty(t, rec.Events)
	assert.Empty(t, r.customizeJSON(context.Background(), &infrav1beta1.VMClone{}), "no customization, no JSON")
}

// TestProviderCustomization_SharesNoMemory: the request's customization is a
// copy; nothing written to it reaches the VMClone's spec.
func TestProviderCustomization_SharesNoMemory(t *testing.T) {
	in := fullCustomization()
	out := providerCustomization(in)
	out.Tags[0] = "changed"
	out.Networks[0].DNS[0] = "changed"
	out.UserData.CloudInit.SecretRef.Name = "changed"
	out.Sysprep.AdminPassword.SecretRef.Name = "changed"
	out.Certificates[0].SecretRef.Name = "changed"
	out.GuestCommands[0].Timeout.Duration = time.Hour
	assert.Equal(t, fullCustomization(), in)
	assert.Nil(t, providerCustomization(nil))
}

// customizationFieldClasses classifies every field of the customization types
// for providerCustomization: "sent" (copied to the request) or "stripped"
// (never sent: nothing reads it and it may hold a secret).
var customizationFieldClasses = map[reflect.Type]map[string]string{
	reflect.TypeFor[infrav1beta1.VMCustomization](): {
		"Hostname": "sent", "Domain": "sent", "TimeZone": "sent", "Networks": "sent", "UserData": "sent",
		"Sysprep": "sent", "Tags": "sent", "GuestCommands": "sent", "Certificates": "sent",
	},
	reflect.TypeFor[infrav1beta1.UserData]():  {"CloudInit": "sent", "Ignition": "sent"},
	reflect.TypeFor[infrav1beta1.CloudInit](): {"Inline": "sent", "SecretRef": "sent"},
	reflect.TypeFor[infrav1beta1.Ignition]():  {"Inline": "stripped", "SecretRef": "sent"},
	reflect.TypeFor[infrav1beta1.SysprepCustomization](): {
		"Enabled": "sent", "ProductKey": "stripped", "Organization": "sent", "Owner": "sent",
		"AdminPassword": "sent", "JoinDomain": "sent", "CustomCommands": "stripped",
	},
	reflect.TypeFor[infrav1beta1.PasswordSpec](): {"Value": "stripped", "SecretRef": "sent", "SecretKey": "sent"},
	reflect.TypeFor[infrav1beta1.DomainJoinSpec](): {
		"Domain": "sent", "Username": "sent", "Password": "sent", "OrganizationalUnit": "sent",
	},
	reflect.TypeFor[infrav1beta1.CertificateSpec](): {
		"Name": "sent", "Data": "stripped", "SecretRef": "sent", "SecretKey": "sent", "Store": "sent",
	},
	reflect.TypeFor[infrav1beta1.GuestCommand](): {
		"Command": "stripped", "Arguments": "stripped", "WorkingDirectory": "sent", "RunAsUser": "sent",
		"Timeout": "sent", "Stage": "sent",
	},
	reflect.TypeFor[infrav1beta1.NetworkCustomization](): {
		"Name": "sent", "IPAddress": "sent", "SubnetMask": "sent", "Gateway": "sent", "DNS": "sent",
		"MACAddress": "sent", "DHCP": "sent",
	},
}

// TestProviderCustomization_EveryFieldClassified fails when a field is added
// to (or removed from) a customization type without being classified here and
// in providerCustomization, so a new field — which may hold a secret — is
// never sent to providers by accident. It also checks the classification
// against providerCustomization: every "sent" leaf of fullCustomization
// arrives, every "stripped" one does not.
func TestProviderCustomization_EveryFieldClassified(t *testing.T) {
	for typ, classes := range customizationFieldClasses {
		var fields []string
		for i := 0; i < typ.NumField(); i++ {
			fields = append(fields, typ.Field(i).Name)
		}
		var classified []string
		for name := range classes {
			classified = append(classified, name)
		}
		sort.Strings(fields)
		sort.Strings(classified)
		assert.Equal(t, classified, fields, "%s: classify every field in customizationFieldClasses and providerCustomization", typ)
	}

	in := reflect.ValueOf(*fullCustomization())
	out := reflect.ValueOf(*providerCustomization(fullCustomization()))
	compareClassified(t, "spec.customization", in, out)
}

// compareClassified walks in (a fully set value) and out (its
// providerCustomization) together: a "stripped" field is zero in out, a "sent"
// leaf equals in's.
func compareClassified(t *testing.T, path string, in, out reflect.Value) {
	t.Helper()
	switch in.Kind() {
	case reflect.Pointer:
		if in.IsNil() {
			return
		}
		require.False(t, out.IsNil(), "%s is not sent", path)
		compareClassified(t, path, in.Elem(), out.Elem())
		return
	case reflect.Slice:
		if in.Type().Elem().Kind() == reflect.Struct {
			require.Equal(t, in.Len(), out.Len(), "%s: every entry is sent", path)
			for i := 0; i < in.Len(); i++ {
				compareClassified(t, path+"[]", in.Index(i), out.Index(i))
			}
			return
		}
	case reflect.Struct:
		classes, ok := customizationFieldClasses[in.Type()]
		if !ok {
			// A leaf struct (metav1.Duration, LocalObjectReference).
			break
		}
		for i := 0; i < in.NumField(); i++ {
			name := in.Type().Field(i).Name
			if classes[name] == "stripped" {
				assert.True(t, out.Field(i).IsZero(), "%s.%s must not be sent", path, name)
				continue
			}
			compareClassified(t, path+"."+name, in.Field(i), out.Field(i))
		}
		return
	}
	assert.Equal(t, in.Interface(), out.Interface(), "%s is sent as it is", path)
}
