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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

func TestParseHostEnrolmentConfig(t *testing.T) {
	cfg, err := ParseHostEnrolmentConfig(HostEnrolmentFlags{}, nil)
	require.NoError(t, err)
	assert.Nil(t, cfg, "no flag: enrolment disabled")

	ca := hostidentitytest.NewCA(t)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, ca.PEM, 0o600))
	full := HostEnrolmentFlags{
		Namespace: "host-agent-pki", Issuer: "Issuer/host-agent-ca", AdminGroup: "enrolment-admins",
		TrustDomain: "cluster.test", CABundleFile: caFile,
	}
	installID := bytes.Repeat([]byte{1}, 16)

	if !devgate.Compiled {
		_, err := ParseHostEnrolmentConfig(full, installID)
		require.ErrorIs(t, err, devgate.ErrNotCompiled, "a release build refuses every enrolment flag")
		return
	}
	cfg, err = ParseHostEnrolmentConfig(full, installID)
	require.NoError(t, err)
	assert.Equal(t, "cert-manager.io/Issuer/host-agent-ca", cfg.CertManagerIssuer.String())
	assert.Nil(t, cfg.ExternalPKIIssuer)

	_, err = ParseHostEnrolmentConfig(full, nil)
	assert.Error(t, err, "the install id (development signer) is required")
	for name, mutate := range map[string]func(*HostEnrolmentFlags){
		"no namespace":       func(f *HostEnrolmentFlags) { f.Namespace = "" },
		"no admin group":     func(f *HostEnrolmentFlags) { f.AdminGroup = "" },
		"bad trust domain":   func(f *HostEnrolmentFlags) { f.TrustDomain = "Not A Domain" },
		"no issuer":          func(f *HostEnrolmentFlags) { f.Issuer = "" },
		"bad issuer":         func(f *HostEnrolmentFlags) { f.Issuer = "just-a-name" },
		"same issuers":       func(f *HostEnrolmentFlags) { f.ExternalPKIIssuer = "cert-manager.io/Issuer/host-agent-ca" },
		"no CA bundle":       func(f *HostEnrolmentFlags) { f.CABundleFile = "" },
		"missing CA bundle":  func(f *HostEnrolmentFlags) { f.CABundleFile = filepath.Join(t.TempDir(), "none") },
		"leaf as CA bundle":  func(f *HostEnrolmentFlags) { f.CABundleFile = writeLeaf(t, ca) },
		"external PKI alone": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := full
			if mutate == nil {
				f.Issuer, f.ExternalPKIIssuer = "", "external-pki.example.test/ExternalIssuer/enterprise"
				_, err := ParseHostEnrolmentConfig(f, installID)
				assert.NoError(t, err)
				return
			}
			mutate(&f)
			_, err := ParseHostEnrolmentConfig(f, installID)
			assert.Error(t, err)
		})
	}
}

// writeLeaf writes a non-CA certificate to a file and returns its path.
func writeLeaf(t *testing.T, ca *hostidentitytest.CA) string {
	t.Helper()
	csr := hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{URIs: []string{"spiffe://x/virtrigaud/host/a/b"}})
	p := filepath.Join(t.TempDir(), "leaf.pem")
	require.NoError(t, os.WriteFile(p, ca.Issue(t, csr, hostidentitytest.IssueOptions{}), 0o600))
	return p
}

// TestHostInventory_AgentHostsRenderPinnedAgentTLS: a Provider that runs
// per-host agents renders each enrolled, unrevoked host with agentTLS read
// from its pins Secret and no SSH material (development builds), and every
// other host as unroutable. A release build renders no agent host.
func TestHostInventory_AgentHostsRenderPinnedAgentTLS(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	prov := clusterProvider("ch-cluster")
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor

	ca := hostidentitytest.NewCA(t)
	pin := strings.Repeat("ab", 32)
	mkHost := func(name, identity string, revoked bool) *infravirtrigaudiov1beta1.Host {
		h := hostCR(name, prov.Name, nil)
		h.UID = types.UID("uid-" + name)
		h.Spec.Endpoint = "grpc://" + name + ":7443"
		h.Spec.Identity = identity
		h.Spec.Revoked = revoked
		return h
	}
	enrolled := mkHost("host-enrolled", "serial-1", false)
	revoked := mkHost("host-revoked", "serial-2", true)
	pending := mkHost("host-pending", "serial-3", false)
	noIdentity := mkHost("host-noidentity", "", false)

	// The Provider's credential Secret carries SSH material; it must never
	// be copied into an agent host.
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(prov, enrolled, revoked, pending, noIdentity, credSecret("test-creds", sshCredData())).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build()
	for _, h := range []*infravirtrigaudiov1beta1.Host{enrolled, revoked} {
		l := loadedEnrolment{state: hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity, Current: &currentEnrolment{
			Record:      enrolmentRecord{EnrolmentID: strings.Repeat("a1", 16), ProviderUID: "0b9a3f2e-5d1c-4f7a-9e6b-2c8d1a4b7e90", InstallID: strings.Repeat("42", 16)},
			KeyPins:     []string{pin},
			CABundle:    ca.PEM,
			ExpectedSAN: "spiffe://cluster.test/virtrigaud/host/default/" + h.Name,
			NotAfter:    metav1.NewTime(time.Now().Add(time.Hour)),
		}}}
		require.NoError(t, saveHostEnrolment(ctx, cli, sch, h, &l))
	}

	r := &ProviderReconciler{Client: cli, Scheme: sch}
	require.NoError(t, r.reconcileHostInventorySecret(ctx, prov))
	sec := &corev1.Secret{}
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: prov.Namespace, Name: r.getHostInventorySecretName(prov)}, sec))
	inv, err := hostsecret.Unmarshal(sec.Data[hostsecret.SecretDataKey])
	require.NoError(t, err)

	if !devgate.Compiled {
		assert.Empty(t, inv.Hosts, "a release build renders no per-host agent host")
		assert.ElementsMatch(t, []string{"host-enrolled", "host-revoked", "host-pending", "host-noidentity"}, inv.UnroutableHostIDs)
		return
	}
	require.Len(t, inv.Hosts, 1)
	h := inv.Hosts[0]
	assert.Equal(t, "host-enrolled", h.ID)
	require.NotNil(t, h.Credentials.AgentTLS)
	assert.Equal(t, []string{pin}, h.Credentials.AgentTLS.KeyPins)
	assert.Equal(t, "spiffe://cluster.test/virtrigaud/host/default/host-enrolled", h.Credentials.AgentTLS.ExpectedSAN)
	assert.Equal(t, ca.PEM, h.Credentials.AgentTLS.CABundle)
	assert.Empty(t, h.Credentials.SSHPrivateKey, "an agent host never carries SSH material")
	assert.Empty(t, h.Credentials.KnownHosts)
	assert.ElementsMatch(t, []string{"host-revoked", "host-pending", "host-noidentity"}, inv.UnroutableHostIDs)
}

// TestLoadHostEnrolment_RefusesAForeignSecret: a Secret with a pins Secret
// name that VirtRigaud does not manage is never read as pins nor
// overwritten.
func TestLoadHostEnrolment_RefusesAForeignSecret(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostEnrolmentSecretName("host-a"), Namespace: "default"},
		Data: map[string][]byte{hostEnrolmentStateKey: []byte(`{}`)}}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(h, foreign).Build()
	_, err := loadHostEnrolment(ctx, cli, h)
	require.ErrorIs(t, err, errForeignPinsSecret)
	_, reason := resolveHostAgentTLS(ctx, cli, h)
	assert.NotEmpty(t, reason)
}
