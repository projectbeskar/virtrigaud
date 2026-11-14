//go:build virtrigaud_dev

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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// policyFiles writes an allow-list (and, with a key, its signature and the
// signers file) to a temporary directory.
type policyFiles struct {
	dir, allowList, signature, signers string
	data                               []byte
}

func writePolicyFiles(t *testing.T, data []byte, signer *ecdsa.PrivateKey, signers ...*ecdsa.PublicKey) policyFiles {
	t.Helper()
	dir := t.TempDir()
	f := policyFiles{dir: dir, allowList: filepath.Join(dir, "allow-list.json"), data: data}
	require.NoError(t, os.WriteFile(f.allowList, data, 0o600))
	if signer != nil {
		d := sha256.Sum256(data)
		sig, err := ecdsa.SignASN1(rand.Reader, signer, d[:])
		require.NoError(t, err)
		f.signature = filepath.Join(dir, "allow-list.sig")
		require.NoError(t, os.WriteFile(f.signature, []byte(base64.StdEncoding.EncodeToString(sig)), 0o600))
	}
	if len(signers) > 0 {
		var pemData []byte
		for _, k := range signers {
			der, err := x509.MarshalPKIXPublicKey(k)
			require.NoError(t, err)
			pemData = append(pemData, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
		}
		f.signers = filepath.Join(dir, "signers.pem")
		require.NoError(t, os.WriteFile(f.signers, pemData, 0o600))
	}
	return f
}

func sha256Hex(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

const policyAllowList = `{"version":1,"required":["cloud-hypervisor"],"components":[{"name":"cloud-hypervisor","version":"1.0.0","alternatives":[{"artifactDigest":"none","files":[{"name":"ch","digest":"sha256:` +
	"0000000000000000000000000000000000000000000000000000000000000001" + `"}]}]}]}`

// The allow-list is read once at start with its pinned digest; changing the
// file afterwards changes nothing the running manager uses.
func TestParseHostInventoryPolicy_LoadsOnce(t *testing.T) {
	f := writePolicyFiles(t, []byte(policyAllowList), nil)
	p, err := ParseHostInventoryPolicy(HostInventoryFlags{AllowListFile: f.allowList, AllowListSHA256: sha256Hex(f.data)})
	require.NoError(t, err)
	require.NoError(t, p.AllowListErr)
	require.NotNil(t, p.AllowList)
	assert.Equal(t, sha256Hex(f.data), p.AllowListSHA256())
	require.NoError(t, os.WriteFile(f.allowList, []byte(`{"version":1,"components":[]}`), 0o600))
	assert.Equal(t, sha256Hex(f.data), p.AllowListSHA256(), "no reload")

	// The digest is in the startup log.
	var logged []string
	p.LogStartup(funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{}))
	assert.Contains(t, strings.Join(logged, "\n"), sha256Hex(f.data))
	// And in the startup metric.
	mfs, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "virtrigaud_component_allow_list" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			found = found || (labels["state"] == "loaded" && labels["sha256"] == sha256Hex(f.data) && m.GetGauge().GetValue() == 1)
		}
	}
	assert.True(t, found, "the allow-list digest is published at start")
}

// The digest pin: an allow-list object deleted and recreated under the same
// name with other content, a missing pin or a missing file is not put in
// force; the manager starts, logs both digests, and serves no Provider whose
// type runs a per-host agent.
func TestParseHostInventoryPolicy_DigestPin(t *testing.T) {
	f := writePolicyFiles(t, []byte(policyAllowList), nil)
	pinned := sha256Hex(f.data)
	recreated := []byte(strings.Replace(policyAllowList, "1.0.0", "1.0.1", 1))
	require.NoError(t, os.WriteFile(f.allowList, recreated, 0o600))
	for name, flags := range map[string]HostInventoryFlags{
		"recreated with other content": {AllowListFile: f.allowList, AllowListSHA256: pinned},
		"pin absent":                   {AllowListFile: f.allowList},
		"file absent":                  {AllowListSHA256: pinned},
		"file missing":                 {AllowListFile: filepath.Join(f.dir, "gone.json"), AllowListSHA256: pinned},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := ParseHostInventoryPolicy(flags)
			require.NoError(t, err, "the manager still starts")
			require.ErrorIs(t, p.AllowListErr, hostinventory.ErrDigestMismatch)
			assert.Nil(t, p.AllowList)
			if name == "recreated with other content" {
				assert.Contains(t, p.AllowListErr.Error(), pinned)
				assert.Contains(t, p.AllowListErr.Error(), sha256Hex(recreated))
			}
			var logged []string
			p.LogStartup(funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{}))
			assert.Contains(t, strings.Join(logged, "\n"), "not in force")
		})
	}
}

// Signed allow-lists: with signers configured, a signature by the release
// identity or by an operator identity in the set puts it in force; a
// signature by an identity outside the set, or a signature file that cannot
// be read, does not; no signature file at all stops the manager.
func TestParseHostInventoryPolicy_Signature(t *testing.T) {
	release, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	operator, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	outsider, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	data := []byte(policyAllowList)
	for name, tc := range map[string]struct {
		signer *ecdsa.PrivateKey
		ok     bool
	}{
		"release identity":  {signer: release, ok: true},
		"operator identity": {signer: operator, ok: true},
		"outside the set":   {signer: outsider},
		"signature missing": {},
	} {
		t.Run(name, func(t *testing.T) {
			f := writePolicyFiles(t, data, tc.signer, &release.PublicKey, &operator.PublicKey)
			if f.signature == "" {
				f.signature = filepath.Join(f.dir, "absent.sig")
			}
			p, err := ParseHostInventoryPolicy(HostInventoryFlags{AllowListFile: f.allowList, AllowListSHA256: sha256Hex(data),
				SignersFile: f.signers, SignatureFile: f.signature})
			require.NoError(t, err)
			if tc.ok {
				require.NoError(t, p.AllowListErr)
				assert.Len(t, p.AllowListSignedBy, 64)
				return
			}
			require.ErrorIs(t, p.AllowListErr, hostinventory.ErrUnverified)
			assert.Nil(t, p.AllowList)
		})
	}
}

// Invalid flag combinations and values stop the manager.
func TestParseHostInventoryPolicy_Invalid(t *testing.T) {
	for name, flags := range map[string]HostInventoryFlags{
		"insecure with an allow-list": {InsecureSkipAllowList: true, AllowListFile: "/x"},
		"signature without signers":   {AllowListFile: "/x", AllowListSHA256: strings.Repeat("0", 64), SignatureFile: "/s"},
		"max age too short":           {MaxAge: "10s"},
		"max age not a duration":      {MaxAge: "soon"},
		"unknown feature":             {RequiredFeatures: "kvm,warp_drive"},
		"none with features":          {RequiredFeatures: "none,kvm"},
		"unknown origin":              {AcceptedOrigins: "anywhere"},
		"unknown tier":                {AcceptedTiers: "pidfd-store,psychic"},
		"empty list after blanks":     {AcceptedTiers: " , "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHostInventoryPolicy(flags)
			require.Error(t, err)
		})
	}
}

// Overrides take effect and are logged at startup with their values; the
// insecure flag is a startup warning.
func TestParseHostInventoryPolicy_Overrides(t *testing.T) {
	p, err := ParseHostInventoryPolicy(HostInventoryFlags{InsecureSkipAllowList: true, MaxAge: "10m", RequiredFeatures: "tun, kvm",
		AcceptedOrigins: "read-only", AcceptedTiers: "pidfs-handle,pidfd-store"})
	require.NoError(t, err)
	assert.True(t, p.SkipAllowList)
	assert.Equal(t, 10*time.Minute, p.MaxAge)
	assert.Equal(t, []string{"kvm", "tun"}, p.RequiredFeatures)
	assert.Equal(t, []providerv1.ManifestOrigin{providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY}, p.AcceptedOrigins)
	assert.ElementsMatch(t, []providerv1.ProcessIdentityTier{providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFD_STORE,
		providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE}, p.AcceptedTiers)
	var logged []string
	p.LogStartup(funcr.New(func(prefix, args string) { logged = append(logged, args) }, funcr.Options{}))
	all := strings.Join(logged, "\n")
	for _, want := range []string{HostInventoryMaxAgeFlagName, "10m0s", "kvm,tun", "read-only", InsecureSkipComponentAllowListFlagName, "WARNING"} {
		assert.Contains(t, all, want)
	}
	none, err := ParseHostInventoryPolicy(HostInventoryFlags{RequiredFeatures: "none"})
	require.NoError(t, err)
	assert.Empty(t, none.RequiredFeatures)
	assert.False(t, errors.Is(none.AllowListErr, hostinventory.ErrDigestMismatch), "no allow-list configured is not an error")
}

// A signers file that is not PEM public keys stops the manager.
func TestParseHostInventoryPolicy_BadSignersFile(t *testing.T) {
	f := writePolicyFiles(t, []byte(policyAllowList), nil)
	bad := filepath.Join(f.dir, "signers.pem")
	require.NoError(t, os.WriteFile(bad, []byte("not a key"), 0o600))
	_, err := ParseHostInventoryPolicy(HostInventoryFlags{AllowListFile: f.allowList, AllowListSHA256: sha256Hex(f.data),
		SignersFile: bad, SignatureFile: filepath.Join(f.dir, "x.sig")})
	require.ErrorIs(t, err, hostinventory.ErrInvalid)
}
