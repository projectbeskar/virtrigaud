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

package hostinventory_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func digest(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}

func hexDigest(s string) string { return "sha256:" + hex.EncodeToString(digest(s)) }

// testAllowList allows cloud-hypervisor 1.0.0 as one artifact with two files,
// or (a second build) with no artifact and one file; it requires
// cloud-hypervisor.
func testAllowList() string {
	return fmt.Sprintf(`{
  "version": 1,
  "required": ["cloud-hypervisor"],
  "components": [
    {"name": "cloud-hypervisor", "version": "1.0.0", "alternatives": [
      {"artifactDigest": %q, "files": [
        {"name": "cloud-hypervisor", "digest": %q},
        {"name": "hypervisor-fw", "digest": %q}]},
      {"artifactDigest": "none", "files": [{"name": "cloud-hypervisor", "digest": %q}]}
    ]}
  ]
}`, hexDigest("image"), hexDigest("ch"), hexDigest("fw"), hexDigest("ch-2"))
}

func component() *providerv1.HostComponent {
	return &providerv1.HostComponent{Name: "cloud-hypervisor", ObservedVersion: "1.0.0", ArtifactSha256: digest("image"),
		Files: []*providerv1.ComponentFile{{Name: "cloud-hypervisor", Sha256: digest("ch")}, {Name: "hypervisor-fw", Sha256: digest("fw")}}}
}

// TestMatch: exact match per (component, observed version).
func TestMatch(t *testing.T) {
	a, err := hostinventory.Parse([]byte(testAllowList()))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Required(); len(got) != 1 || got[0] != "cloud-hypervisor" {
		t.Fatalf("required = %v", got)
	}
	cases := map[string]struct {
		edit func(*providerv1.HostComponent)
		want hostinventory.Result
	}{
		"exact match": {want: hostinventory.Allowed},
		"second build without an artifact": {edit: func(c *providerv1.HostComponent) {
			c.ArtifactSha256 = nil
			c.Files = []*providerv1.ComponentFile{{Name: "cloud-hypervisor", Sha256: digest("ch-2")}}
		}, want: hostinventory.Allowed},
		"artifact differs":        {edit: func(c *providerv1.HostComponent) { c.ArtifactSha256 = digest("other") }, want: hostinventory.NotAllowed},
		"artifact absent":         {edit: func(c *providerv1.HostComponent) { c.ArtifactSha256 = nil }, want: hostinventory.NotAllowed},
		"one file digest differs": {edit: func(c *providerv1.HostComponent) { c.Files[1].Sha256 = digest("other") }, want: hostinventory.NotAllowed},
		"missing file name":       {edit: func(c *providerv1.HostComponent) { c.Files = c.Files[:1] }, want: hostinventory.NotAllowed},
		"extra file name": {edit: func(c *providerv1.HostComponent) {
			c.Files = append(c.Files, &providerv1.ComponentFile{Name: "z-extra", Sha256: digest("z")})
		}, want: hostinventory.NotAllowed},
		"renamed file":    {edit: func(c *providerv1.HostComponent) { c.Files[1].Name = "hypervisor-fw2" }, want: hostinventory.NotAllowed},
		"other version":   {edit: func(c *providerv1.HostComponent) { c.ObservedVersion = "1.0.1" }, want: hostinventory.NotListed},
		"other component": {edit: func(c *providerv1.HostComponent) { c.Name = "firmware" }, want: hostinventory.NotListed},
		"no digests reported": {edit: func(c *providerv1.HostComponent) { c.ArtifactSha256, c.Files = nil, nil },
			want: hostinventory.NotChecked},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := component()
			if tc.edit != nil {
				tc.edit(c)
			}
			if got := a.Match(c); got != tc.want {
				t.Fatalf("Match = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestParseRefuses: malformed files are refused.
func TestParseRefuses(t *testing.T) {
	d := hexDigest("x")
	for name, body := range map[string]string{
		"not JSON":            `{`,
		"unknown field":       `{"version":1,"components":[],"extra":1}`,
		"trailing data":       `{"version":1,"components":[]} {}`,
		"format version 2":    `{"version":2,"components":[]}`,
		"malformed name":      fmt.Sprintf(`{"version":1,"components":[{"name":"CH","version":"1","alternatives":[{"artifactDigest":%q,"files":[]}]}]}`, d),
		"malformed version":   fmt.Sprintf(`{"version":1,"components":[{"name":"ch","version":"1 0","alternatives":[{"artifactDigest":%q,"files":[]}]}]}`, d),
		"repeated entry":      fmt.Sprintf(`{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":%q}]},{"name":"ch","version":"1","alternatives":[{"artifactDigest":%q}]}]}`, d, d),
		"no alternative":      `{"version":1,"components":[{"name":"ch","version":"1","alternatives":[]}]}`,
		"uppercase digest":    fmt.Sprintf(`{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":%q}]}]}`, strings.ToUpper(d)),
		"other digest form":   `{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":"md5:00"}]}]}`,
		"empty alternative":   `{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":"none","files":[]}]}]}`,
		"file name is a path": fmt.Sprintf(`{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":"none","files":[{"name":"/bin/ch","digest":%q}]}]}]}`, d),
		"repeated file":       fmt.Sprintf(`{"version":1,"components":[{"name":"ch","version":"1","alternatives":[{"artifactDigest":"none","files":[{"name":"a","digest":%q},{"name":"a","digest":%q}]}]}]}`, d, d),
		"repeated required":   `{"version":1,"required":["ch","ch"],"components":[]}`,
		"oversized":           `{"version":1,"components":[]}` + strings.Repeat(" ", hostinventory.MaxFileBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hostinventory.Parse([]byte(body)); !errors.Is(err, hostinventory.ErrInvalid) {
				t.Fatalf("Parse = %v, want ErrInvalid", err)
			}
		})
	}
}

func pinOf(data []byte) string {
	d := sha256.Sum256(data)
	return hex.EncodeToString(d[:])
}

// TestLoadDigestPin: the file loads only with the pinned digest; an object
// deleted and recreated under the same name with other content, or a
// missing pin, is refused, and the error names both digests.
func TestLoadDigestPin(t *testing.T) {
	data := []byte(testAllowList())
	res, err := hostinventory.Load(hostinventory.LoadOptions{Data: data, PinnedSHA256: pinOf(data)})
	if err != nil || res.AllowList.SHA256() != pinOf(data) || res.SignedBy != "" {
		t.Fatalf("Load = %+v, %v", res, err)
	}
	recreated := []byte(strings.Replace(testAllowList(), `"1.0.0"`, `"1.0.1"`, 1))
	_, err = hostinventory.Load(hostinventory.LoadOptions{Data: recreated, PinnedSHA256: pinOf(data)})
	if !errors.Is(err, hostinventory.ErrDigestMismatch) || !strings.Contains(err.Error(), pinOf(data)) ||
		!strings.Contains(err.Error(), pinOf(recreated)) {
		t.Fatalf("Load of recreated content = %v, want ErrDigestMismatch naming both digests", err)
	}
	for _, pin := range []string{"", "00", strings.ToUpper(pinOf(data))} {
		if _, err := hostinventory.Load(hostinventory.LoadOptions{Data: data, PinnedSHA256: pin}); !errors.Is(err, hostinventory.ErrDigestMismatch) {
			t.Fatalf("Load with pin %q = %v, want ErrDigestMismatch", pin, err)
		}
	}
}

func pemOf(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func ecdsaSign(t *testing.T, k *ecdsa.PrivateKey, data []byte) []byte {
	t.Helper()
	d := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// disableNetwork makes every dial through the default resolver and the
// default HTTP transport fail for the rest of the test: the signature check
// must not need either.
func disableNetwork(t *testing.T) {
	t.Helper()
	fail := func(context.Context, string, string) (net.Conn, error) {
		t.Error("the allow-list check dialed the network")
		return nil, errors.New("network disabled")
	}
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: fail}
	tr, _ := http.DefaultTransport.(*http.Transport)
	var oldDial func(context.Context, string, string) (net.Conn, error)
	if tr != nil {
		oldDial = tr.DialContext
		tr.DialContext = fail
	}
	t.Cleanup(func() {
		net.DefaultResolver = oldResolver
		if tr != nil {
			tr.DialContext = oldDial
		}
	})
}

// TestLoadSignature: with signers configured the file must carry a detached
// signature by one of them: the release identity or an operator identity in
// the set verifies; an identity outside the set, a missing or malformed
// signature does not. Verification uses only the configured keys.
func TestLoadSignature(t *testing.T) {
	disableNetwork(t)
	data := []byte(testAllowList())
	release, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	operatorPub, operator, _ := ed25519.GenerateKey(rand.Reader)
	outsider, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signersPEM := append(pemOf(t, &release.PublicKey), pemOf(t, operatorPub)...)
	signers, err := hostinventory.ParseSigners(signersPEM)
	if err != nil || len(signers) != 2 {
		t.Fatalf("ParseSigners = %v, %v", signers, err)
	}
	load := func(sig []byte) (hostinventory.LoadResult, error) {
		return hostinventory.Load(hostinventory.LoadOptions{Data: data, PinnedSHA256: pinOf(data), Signers: signers, Signature: sig})
	}
	if res, err := load(ecdsaSign(t, release, data)); err != nil || res.SignedBy != signers[0].Fingerprint {
		t.Fatalf("release signature: %+v, %v", res, err)
	}
	opSig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(operator, data)))
	if res, err := load(opSig); err != nil || res.SignedBy != signers[1].Fingerprint {
		t.Fatalf("operator signature: %+v, %v", res, err)
	}
	for name, sig := range map[string][]byte{
		"outside the set":   ecdsaSign(t, outsider, data),
		"unsigned":          nil,
		"not base64":        []byte("%%%"),
		"over other bytes":  ecdsaSign(t, release, []byte("other")),
		"oversized":         []byte(strings.Repeat("A", hostinventory.MaxSignatureBytes+1)),
		"truncated ed25519": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(operator, data)[:10])),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(sig); !errors.Is(err, hostinventory.ErrUnverified) {
				t.Fatalf("Load = %v, want ErrUnverified", err)
			}
		})
	}
	// The digest pin is checked before the signature.
	if _, err := hostinventory.Load(hostinventory.LoadOptions{Data: data, PinnedSHA256: pinOf([]byte("x")), Signers: signers,
		Signature: ecdsaSign(t, release, data)}); !errors.Is(err, hostinventory.ErrDigestMismatch) {
		t.Fatalf("Load = %v, want ErrDigestMismatch", err)
	}
}

// TestParseSignersRefuses: only PEM ECDSA P-256 and Ed25519 public keys.
func TestParseSignersRefuses(t *testing.T) {
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	one := pemOf(t, &p256.PublicKey)
	for name, data := range map[string][]byte{
		"empty":         nil,
		"P-384 key":     pemOf(t, &p384.PublicKey),
		"repeated key":  append(append([]byte(nil), one...), one...),
		"private block": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}}),
		"garbage":       append(append([]byte(nil), one...), []byte("garbage")...),
		"bad DER":       pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1, 2}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hostinventory.ParseSigners(data); !errors.Is(err, hostinventory.ErrInvalid) {
				t.Fatalf("ParseSigners = %v, want ErrInvalid", err)
			}
		})
	}
}
