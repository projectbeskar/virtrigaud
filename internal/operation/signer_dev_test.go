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

package operation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// writeKeyFile writes data to a 0600 file and returns its path.
func writeKeyFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "signer.pem")
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}

// devVerifier verifies grants of s at the clock now.
func devVerifier(t *testing.T, anchor *x509.Certificate, installID []byte, now func() time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{Anchors: []*x509.Certificate{anchor}, InstallID: installID,
		ProviderUID: testProviderUID, HostID: testHostID, EnrolmentID: testEnrolment, Now: now})
	require.NoError(t, err)
	return v
}

// mintAndVerify mints a POWER_ON grant with s at now and verifies it with v.
func mintAndVerify(t *testing.T, s Signer, v *Verifier, now time.Time) error {
	t.Helper()
	req, err := proto.Marshal(testRequest(providerv1.OperationKind_OPERATION_KIND_POWER_ON))
	require.NoError(t, err)
	g, err := MintOperationGrant(context.Background(), s, MintRequest{
		Identity:   testIdentity(providerv1.OperationKind_OPERATION_KIND_POWER_ON),
		DigestSalt: bytesOf(0x5A, SaltLen), Request: req, IssuedAt: now,
	})
	require.NoError(t, err)
	_, err = v.Verify(providerv1.Provider_Power_FullMethodName,
		&providerv1.PowerRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: req, Grant: g}})
	return err
}

// TestDevBuild_SignerCompiled: a development build has the signer.
func TestDevBuild_SignerCompiled(t *testing.T) {
	require.True(t, SignerCompiled)
	s, err := LoadSigner("")
	require.NoError(t, err)
	assert.Nil(t, s)
}

// TestDevSigner_LoadMintVerify: a generated key file loads, and its grants
// verify against its anchor with its install id.
func TestDevSigner_LoadMintVerify(t *testing.T) {
	data, err := GenerateDevSignerKey()
	require.NoError(t, err)
	s, err := LoadSigner(writeKeyFile(t, data))
	require.NoError(t, err)
	ds := mustAs[*DevSigner](t, s)
	assert.Equal(t, uint64(1), ds.AuthorityGeneration())
	assert.Len(t, ds.InstallID(), IDLen)
	now := time.Now()
	require.NoError(t, mintAndVerify(t, ds, devVerifier(t, ds.Anchor(), ds.InstallID(), func() time.Time { return now }), now))
}

// TestDevSigner_StableAcrossRestart: a signer loaded again from the same key
// file (a manager restart) signs grants that verify against the anchor the
// first one published.
func TestDevSigner_StableAcrossRestart(t *testing.T) {
	data, err := GenerateDevSignerKey()
	require.NoError(t, err)
	path := writeKeyFile(t, data)
	first, err := LoadDevSignerKeyFile(path)
	require.NoError(t, err)
	second, err := LoadDevSignerKeyFile(path)
	require.NoError(t, err)
	now := time.Now()
	v := devVerifier(t, first.Anchor(), first.InstallID(), func() time.Time { return now })
	require.NoError(t, mintAndVerify(t, second, v, now))

	// Another key's grants do not verify against it.
	other, err := GenerateDevSignerKey()
	require.NoError(t, err)
	third, err := LoadDevSignerKeyFile(writeKeyFile(t, other))
	require.NoError(t, err)
	third.installID = first.InstallID()
	err = mintAndVerify(t, third, v, now)
	r, ok := AsRefusal(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, RuleChain, r.Rule)
}

// TestDevSigner_RotatesTheLeaf: past the leaf's lifetime the signer mints a
// new leaf (and issuer) and its grants still verify.
func TestDevSigner_RotatesTheLeaf(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	s, err := NewDevSigner(key, bytesOf(7, IDLen), clock)
	require.NoError(t, err)
	v := devVerifier(t, s.Anchor(), s.InstallID(), clock)
	require.NoError(t, mintAndVerify(t, s, v, clock()))
	firstLeaf := s.leafCert.Raw
	for _, step := range []time.Duration{58 * time.Minute, 2 * time.Hour, 30 * time.Hour} {
		mu.Lock()
		now = now.Add(step)
		mu.Unlock()
		require.NoError(t, mintAndVerify(t, s, v, clock()), "after %s", step)
	}
	assert.NotEqual(t, firstLeaf, s.leafCert.Raw)
}

// TestDevSigner_ConcurrentSign: concurrent signing is race-free (run with
// -race) and every grant verifies.
func TestDevSigner_ConcurrentSign(t *testing.T) {
	data, err := GenerateDevSignerKey()
	require.NoError(t, err)
	s, err := LoadDevSignerKeyFile(writeKeyFile(t, data))
	require.NoError(t, err)
	now := time.Now()
	v := devVerifier(t, s.Anchor(), s.InstallID(), func() time.Time { return now })
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mintAndVerify(t, s, v, now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

// TestDevSigner_RefusesBadKeyFiles: the key file has exactly one P-256 key
// and one 16-byte install id, nothing else; errors never quote the content.
func TestDevSigner_RefusesBadKeyFiles(t *testing.T) {
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	keyBlock := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: DevSignerKeyPEMType, Bytes: der})
	}
	idBlock := func(n int) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: DevSignerInstallIDPEMType, Bytes: bytesOf(3, n)})
	}
	join := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for name, data := range map[string][]byte{
		"empty":            {},
		"no install id":    keyBlock(p256),
		"no key":           idBlock(IDLen),
		"P-384 key":        join(keyBlock(p384), idBlock(IDLen)),
		"short install id": join(keyBlock(p256), idBlock(8)),
		"two keys":         join(keyBlock(p256), keyBlock(p256), idBlock(IDLen)),
		"two install ids":  join(keyBlock(p256), idBlock(IDLen), idBlock(IDLen)),
		"other block":      join(keyBlock(p256), idBlock(IDLen), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}})),
		"trailing data":    join(keyBlock(p256), idBlock(IDLen), []byte("secret-trailer")),
		"key not PKCS8":    join(pem.EncodeToMemory(&pem.Block{Type: DevSignerKeyPEMType, Bytes: []byte("secret-junk")}), idBlock(IDLen)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSigner(writeKeyFile(t, data))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret-")
			assert.Contains(t, err.Error(), DevSignerKeyFileFlagName)
		})
	}
	_, err = LoadSigner(filepath.Join(t.TempDir(), "missing.pem"))
	assert.Error(t, err)

	_, err = NewDevSigner(p384, bytesOf(1, IDLen), nil)
	assert.Error(t, err)
	_, err = NewDevSigner(p256, bytesOf(1, 3), nil)
	assert.Error(t, err)
	_, err = EncodeDevSignerKey(p384, bytesOf(1, IDLen))
	assert.Error(t, err)
}

// TestDevSigner_RefusesLongValidity and a cancelled context.
func TestDevSigner_SignRefusals(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	s, err := NewDevSigner(key, bytesOf(7, IDLen), nil)
	require.NoError(t, err)
	_, err = s.Sign(context.Background(), []byte("body"), time.Now().Add(2*MaxGrantLifetime))
	assert.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Sign(ctx, []byte("body"), time.Now().Add(time.Minute))
	assert.Error(t, err)
}

// TestDevSigner_LeafProfile: the development signer's leaf has the
// grant-signing profile (not a CA, digitalSignature only, no EKU) and its
// issuer is a path length 0 CA that may only sign certificates.
func TestDevSigner_LeafProfile(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	s, err := NewDevSigner(key, bytesOf(7, IDLen), nil)
	require.NoError(t, err)
	sig, err := s.Sign(context.Background(), []byte("body"), time.Now().Add(time.Minute))
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(sig.Chain[0])
	require.NoError(t, err)
	issuer, err := x509.ParseCertificate(sig.Chain[1])
	require.NoError(t, err)
	assert.True(t, isGrantSigningLeaf(leaf))
	assert.Empty(t, leaf.ExtKeyUsage)
	assert.Equal(t, x509.KeyUsageDigitalSignature, leaf.KeyUsage)
	assert.True(t, issuer.IsCA && issuer.MaxPathLenZero && issuer.MaxPathLen == 0)
	assert.Equal(t, x509.KeyUsageCertSign, issuer.KeyUsage)
}

// TestDevSigner_LowS: every signature the development signer emits is in
// low-S form.
func TestDevSigner_LowS(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	s, err := NewDevSigner(key, bytesOf(7, IDLen), nil)
	require.NoError(t, err)
	for i := 0; i < 64; i++ {
		sig, err := s.Sign(context.Background(), []byte{byte(i)}, time.Now().Add(time.Minute))
		require.NoError(t, err)
		assert.True(t, isLowS(sig.Signature, elliptic.P256()))
	}
}

// TestDevSigner_RefusesOpenKeyFileModes: a key file that group or others can
// access is refused before it is read; 0600 and 0400 load.
func TestDevSigner_RefusesOpenKeyFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix mode bits")
	}
	data, err := GenerateDevSignerKey()
	require.NoError(t, err)
	for mode, ok := range map[os.FileMode]bool{0o600: true, 0o400: true, 0o640: false, 0o644: false, 0o604: false, 0o660: false} {
		t.Run(mode.String(), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "signer.pem")
			require.NoError(t, os.WriteFile(p, data, 0o600))
			require.NoError(t, os.Chmod(p, mode))
			_, err := LoadSigner(p)
			if ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, "group and others must have no access")
			}
		})
	}
}
