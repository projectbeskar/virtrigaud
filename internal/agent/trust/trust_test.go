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

package trust_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

func writeAll(t *testing.T, p *trusttest.PKI) (trustDir, pkiDir string) {
	t.Helper()
	root := t.TempDir()
	trustDir, pkiDir = filepath.Join(root, "trust"), filepath.Join(root, "pki")
	p.WriteTrust(t, trustDir)
	p.WritePKI(t, pkiDir)
	return trustDir, pkiDir
}

func TestLoadConfigAndIdentity(t *testing.T) {
	p := trusttest.New(t)
	trustDir, pkiDir := writeAll(t, p)
	cfg, err := trust.LoadConfig(trustDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0] != trusttest.ProviderIdentity() {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	id, err := trust.LoadIdentity(pkiDir, cfg, time.Now(), nil, p.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	if id.HostURI != p.HostURI || id.HostID() != trusttest.HostName || id.HostNamespace != trusttest.HostNS || !id.Record.Equal(p.Record) {
		t.Fatalf("identity = %+v", id)
	}
	if !id.SameEnrolment(id) {
		t.Fatal("identity is not the same enrolment as itself")
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, dir string, p *trusttest.PKI)
	}{
		{"missing host CA", func(t *testing.T, dir string, _ *trusttest.PKI) { rm(t, dir, trust.FileHostCA) }},
		{"missing provider CA", func(t *testing.T, dir string, _ *trusttest.PKI) { rm(t, dir, trust.FileProviderCA) }},
		{"missing grant anchors", func(t *testing.T, dir string, _ *trusttest.PKI) { rm(t, dir, trust.FileGrantAnchors) }},
		{"missing allow-list", func(t *testing.T, dir string, _ *trusttest.PKI) { rm(t, dir, trust.FileProviderSANs) }},
		{"empty allow-list", func(t *testing.T, dir string, _ *trusttest.PKI) { write(t, dir, trust.FileProviderSANs, "# none\n\n") }},
		{"allow-list entry with a UID", func(t *testing.T, dir string, _ *trusttest.PKI) {
			write(t, dir, trust.FileProviderSANs, trusttest.ProviderURI("abc")+"\n")
		}},
		{"allow-list entry of a host", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderSANs, p.HostURI+"\n")
		}},
		{"allow-list repeated entry", func(t *testing.T, dir string, _ *trusttest.PKI) {
			s := trusttest.ProviderIdentity().String()
			write(t, dir, trust.FileProviderSANs, s+"\n"+s+"\n")
		}},
		{"provider CA is not a CA", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderCA, string(p.HostCertPEM))
		}},
		{"grant anchor equals the host CA", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileGrantAnchors, string(p.HostCA.PEM))
		}},
		{"host CA issued by the grant anchor", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileHostCA, string(subCA(t, p.GrantCA)))
		}},
		{"grant anchor issued by the host CA", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileGrantAnchors, string(subCA(t, p.HostCA)))
		}},
		{"provider CA equals the host CA", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderCA, string(p.HostCA.PEM))
		}},
		{"provider CA equals the grant anchor", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderCA, string(p.GrantCA.PEM))
		}},
		{"provider CA issued by the host CA", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderCA, string(subCA(t, p.HostCA)))
		}},
		{"provider CA issued by the grant anchor", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileProviderCA, string(subCA(t, p.GrantCA)))
		}},
		{"provider CA and host CA share an issuer", func(t *testing.T, dir string, _ *trusttest.PKI) {
			root := hostidentitytest.NewCA(t)
			write(t, dir, trust.FileProviderCA, string(subCA(t, root)))
			write(t, dir, trust.FileHostCA, string(subCA(t, root)))
		}},
		{"grant anchor and host CA share an issuer", func(t *testing.T, dir string, _ *trusttest.PKI) {
			root := hostidentitytest.NewCA(t)
			write(t, dir, trust.FileGrantAnchors, string(subCA(t, root)))
			write(t, dir, trust.FileHostCA, string(subCA(t, root)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := trusttest.New(t)
			dir, _ := writeAll(t, p)
			tc.edit(t, dir, p)
			if _, err := trust.LoadConfig(dir, nil); !errors.Is(err, trust.ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
		})
	}
}

func TestLoadIdentityRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, dir string, p *trusttest.PKI)
		now  time.Time
	}{
		{"no signer", func(*testing.T, string, *trusttest.PKI) {}, time.Time{}},
		{"missing certificate", func(t *testing.T, dir string, _ *trusttest.PKI) { rm(t, dir, trust.FileHostCert) }, time.Time{}},
		{"certificate for another key", func(t *testing.T, dir string, p *trusttest.PKI) {
			other := hostidentitytest.NewKey(t)
			write(t, dir, trust.FileHostCert, string(p.IssueHost(t, other, hostidentity.RequestURIs(p.HostURI, p.Record), hostidentitytest.IssueOptions{})))
		}, time.Time{}},
		{"certificate from another issuer", func(t *testing.T, dir string, p *trusttest.PKI) {
			csr := hostidentitytest.NewCSR(t, p.HostKey, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(p.HostURI, p.Record)})
			write(t, dir, trust.FileHostCert, string(hostidentitytest.NewCA(t).Issue(t, csr, hostidentitytest.IssueOptions{})))
		}, time.Time{}},
		{"no server authentication usage", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileHostCert, string(p.IssueHost(t, p.HostKey, hostidentity.RequestURIs(p.HostURI, p.Record),
				hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})))
		}, time.Time{}},
		{"no enrolment record", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileHostCert, string(p.IssueHost(t, p.HostKey, []string{p.HostURI}, hostidentitytest.IssueOptions{})))
		}, time.Time{}},
		{"no host identity", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileHostCert, string(p.IssueHost(t, p.HostKey, p.Record.URIs(), hostidentitytest.IssueOptions{})))
		}, time.Time{}},
		{"two certificates", func(t *testing.T, dir string, p *trusttest.PKI) {
			write(t, dir, trust.FileHostCert, string(p.HostCertPEM)+string(p.HostCA.PEM))
		}, time.Time{}},
		{"expired", func(*testing.T, string, *trusttest.PKI) {}, time.Now().Add(60 * 24 * time.Hour)},
		{"signer is not P-256", func(*testing.T, string, *trusttest.PKI) {}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := trusttest.New(t)
			trustDir, pkiDir := writeAll(t, p)
			tc.edit(t, pkiDir, p)
			cfg, err := trust.LoadConfig(trustDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			now := tc.now
			if now.IsZero() {
				now = time.Now()
			}
			var signer crypto.Signer = p.HostKey
			switch tc.name {
			case "no signer":
				signer = nil
			case "signer is not P-256":
				k, kerr := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
				if kerr != nil {
					t.Fatal(kerr)
				}
				signer = k
			}
			if _, err := trust.LoadIdentity(pkiDir, cfg, now, nil, signer); !errors.Is(err, trust.ErrIdentity) {
				t.Fatalf("err = %v, want ErrIdentity", err)
			}
		})
	}
}

func TestAuthorizeProvider(t *testing.T) {
	p := trusttest.New(t)
	dir, _ := writeAll(t, p)
	cfg, err := trust.LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok := p.ProviderCert(t, nil, trusttest.ProviderURI(trusttest.ProviderUID))
	id, uid, err := cfg.AuthorizeProvider(ok.Leaf)
	if err != nil || id != trusttest.ProviderIdentity() || uid != trusttest.ProviderUID {
		t.Fatalf("allowed provider: %+v %q %v", id, uid, err)
	}
	for name, uri := range map[string]string{
		"no uid":         trusttest.ProviderIdentity().String(),
		"other name":     "spiffe://example.test/virtrigaud/provider/infra/other/uid-1",
		"other domain":   "spiffe://other.test/virtrigaud/provider/infra/ch/uid-1",
		"host identity":  p.HostURI,
		"bad uid":        trusttest.ProviderIdentity().String() + "/UPPER",
		"extra segment":  trusttest.ProviderURI("uid-1") + "/x",
		"query":          trusttest.ProviderURI("uid-1") + "?a=b",
		"other scheme":   "https://example.test/virtrigaud/provider/infra/ch/uid-1",
		"escaped path":   "spiffe://example.test/virtrigaud/provider/infra/ch%2Fx/uid-1",
		"user info":      "spiffe://u@example.test/virtrigaud/provider/infra/ch/uid-1",
		"trailing slash": trusttest.ProviderURI("uid-1") + "/",
	} {
		c := p.ProviderCert(t, nil, uri)
		if _, _, err := cfg.AuthorizeProvider(c.Leaf); !errors.Is(err, trust.ErrProviderRefused) {
			t.Errorf("%s (%s): err = %v", name, uri, err)
		}
	}
	two := *ok.Leaf
	u, _ := url.Parse(trusttest.ProviderURI("uid-2"))
	two.URIs = append(append([]*url.URL(nil), ok.Leaf.URIs...), u)
	if _, _, err := cfg.AuthorizeProvider(&two); err == nil {
		t.Error("two URI identities accepted")
	}
}

// subCA issues a CA certificate from parent and returns it in PEM.
func subCA(t *testing.T, parent *hostidentitytest.CA) []byte {
	t.Helper()
	key := hostidentitytest.NewKey(t)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{CommonName: "sub"})
	out := parent.Issue(t, csr, hostidentitytest.IssueOptions{IsCA: true, ExtKeyUsage: []x509.ExtKeyUsage{}})
	block, _ := pem.Decode(out)
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatal(err)
	}
	return out
}

func rm(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
