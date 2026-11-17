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

// Command agent-labpki provisions a throw-away identity for a per-host agent
// on a disposable test host, and probes a running agent. It is a test tool,
// not part of any release: the keys it writes are lab keys.
//
//	agent-labpki gen   -out DIR [-host NAME] [-trust-domain TD]
//	agent-labpki hello -dir DIR -addr HOST:PORT
//	agent-labpki hostinfo -dir DIR -addr HOST:PORT
//	agent-labpki burst -dir DIR -addr HOST:PORT [-n N] [-workers W]
//
// gen writes, under DIR:
//
//	trust/host-ca.pem, trust/provider-ca.pem, trust/grant-anchors.pem,
//	trust/provider-sans      the trust configuration (install root-owned)
//	signer/host.key          the host private key (for the signer's user)
//	pki/host.crt             the host certificate (for the agent's user)
//	host-id                  the provisioning marker (the host URI)
//	provider/client.crt, provider/client.key, provider/host-ca.pem
//	                         a Provider client identity for hello
//
// The host certificate carries a first-epoch enrolment record for a fixed
// Provider UID, so a development agent enrols a fresh store from it.
//
// hello dials the agent as the allowed Provider over TLS 1.3, verifies the
// agent's certificate against the host identity issuer, calls Hello and
// prints one JSON line: whether the answer was signed, and the decoded
// HostAnswer.
//
// hostinfo does the same with GetHostInfo, whose signed HostAnswer carries
// the host inventory (feature probes, components).
//
// burst sends N VM lifecycles (Create, PowerOff, Delete: 3N operations) over W
// concurrent workers to an agent running the development fake executor,
// minting each operation's grant from the lab grant anchor as the manager
// would (gen keeps that anchor's key in provider/grant-ca.key and the
// enrolment record in provider/record.json), waits for each task to end and
// prints one JSON line of counts.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Fixed names of the lab identity.
const (
	defaultTrustDomain = "lab.example.test"
	hostNamespace      = "hosts"
	providerNamespace  = "infra"
	providerName       = "ch"
	providerUID        = "0b7a1c3e-1111-4222-8333-944455556666"
)

// Output layout.
const (
	dirTrust     = "trust"
	dirSigner    = "signer"
	dirPKI       = "pki"
	dirProvider  = "provider"
	fileHostID   = "host-id"
	fileClient   = "client.crt"
	fileKey      = "client.key"
	fileGrantCA  = "grant-ca.pem"
	fileGrantKey = "grant-ca.key"
	fileRecord   = "record.json"
)

// labRecord is what burst needs of the enrolment: the host name and the
// record the host certificate carries.
type labRecord struct {
	// Host is the host's name in its URI identity.
	Host string `json:"host"`
	// ProviderUID is the Provider the host is enrolled with.
	ProviderUID string `json:"providerUid"`
	// EnrolmentID is the enrolment identifier.
	EnrolmentID []byte `json:"enrolmentId"`
	// InstallID is the manager installation identifier.
	InstallID []byte `json:"installId"`
}

// fatalT adapts the test helpers to a command: a failure ends the process.
type fatalT struct{}

// Helper implements hostidentitytest.T.
func (fatalT) Helper() {}

// Fatalf implements hostidentitytest.T.
func (fatalT) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agent-labpki: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-labpki gen|hello|hostinfo|burst [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = gen(os.Args[2:])
	case "hello":
		err = hello(os.Args[2:], false)
	case "hostinfo":
		err = hello(os.Args[2:], true)
	case "burst":
		err = burst(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-labpki:", err)
		os.Exit(1)
	}
}

// gen writes a complete lab identity under -out.
func gen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "", "output directory (created)")
	host := fs.String("host", "host-a", "the host's name in its URI identity")
	td := fs.String("trust-domain", defaultTrustDomain, "the trust domain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("gen: -out is required")
	}
	t := fatalT{}
	hostCA, providerCA, grantCA := hostidentitytest.NewCA(t), hostidentitytest.NewCA(t), hostidentitytest.NewCA(t)
	hostKey := hostidentitytest.NewKey(t)
	hostURI, err := hostidentity.HostURI(*td, hostNamespace, *host)
	if err != nil {
		return fmt.Errorf("host URI: %w", err)
	}
	rec := hostidentity.Record{
		EnrolmentID: randomID(), ProviderUID: providerUID, InstallID: randomID(), Mode: hostidentity.ModeIdentity,
	}
	csr := hostidentitytest.NewCSR(t, hostKey, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(hostURI, rec)})
	hostCert := hostCA.Issue(t, csr, hostidentitytest.IssueOptions{})

	pid := trust.ProviderIdentity{TrustDomain: *td, Namespace: providerNamespace, Name: providerName}
	clientKey := hostidentitytest.NewKey(t)
	clientCSR := hostidentitytest.NewCSR(t, clientKey, hostidentitytest.CSROptions{URIs: []string{pid.String() + "/" + providerUID}})
	clientCert := providerCA.Issue(t, clientCSR, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})

	hostKeyPEM, err := keyPEM(hostKey)
	if err != nil {
		return err
	}
	clientKeyPEM, err := keyPEM(clientKey)
	if err != nil {
		return err
	}
	grantKeyPEM, err := keyPEM(grantCA.Key)
	if err != nil {
		return err
	}
	recJSON, err := json.Marshal(labRecord{Host: *host, ProviderUID: rec.ProviderUID, EnrolmentID: rec.EnrolmentID,
		InstallID: rec.InstallID})
	if err != nil {
		return fmt.Errorf("encode the record: %w", err)
	}
	files := []struct {
		path string
		data []byte
	}{
		{filepath.Join(dirTrust, trust.FileHostCA), hostCA.PEM},
		{filepath.Join(dirTrust, trust.FileProviderCA), providerCA.PEM},
		{filepath.Join(dirTrust, trust.FileGrantAnchors), grantCA.PEM},
		{filepath.Join(dirTrust, trust.FileProviderSANs), []byte("# Providers this host serves\n" + pid.String() + "\n")},
		{filepath.Join(dirSigner, trust.FileHostKey), hostKeyPEM},
		{filepath.Join(dirPKI, trust.FileHostCert), hostCert},
		{fileHostID, []byte(hostURI + "\n")},
		{filepath.Join(dirProvider, fileClient), clientCert},
		{filepath.Join(dirProvider, fileKey), clientKeyPEM},
		{filepath.Join(dirProvider, trust.FileHostCA), hostCA.PEM},
		{filepath.Join(dirProvider, fileGrantCA), grantCA.PEM},
		{filepath.Join(dirProvider, fileGrantKey), grantKeyPEM},
		{filepath.Join(dirProvider, fileRecord), recJSON},
	}
	for _, f := range files {
		p := filepath.Join(*out, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, f.data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	fmt.Println(hostURI)
	return nil
}

// randomID returns a fresh enrolment-record identifier.
func randomID() []byte {
	id, err := hostidentity.NewEnrolmentID(rand.Reader)
	if err != nil {
		fatalT{}.Fatalf("draw an identifier: %v", err)
	}
	return id
}

// keyPEM encodes a private key as PKCS #8 PEM.
func keyPEM(key any) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// helloResult is hello's output line.
type helloResult struct {
	// Signed reports whether the agent returned a signed HostAnswer.
	Signed bool `json:"signed"`
	// Answer is the decoded HostAnswer, when signed.
	Answer json.RawMessage `json:"answer,omitempty"`
	// Error is the call's error, if any.
	Error string `json:"error,omitempty"`
}

// hello calls Hello (or, with info, GetHostInfo) on a running agent as the
// lab Provider.
func hello(args []string, info bool) error {
	fs := flag.NewFlagSet("hello", flag.ContinueOnError)
	dir := fs.String("dir", "", "the directory gen wrote")
	addr := fs.String("addr", "127.0.0.1:9460", "the agent's address")
	timeout := fs.Duration("timeout", 5*time.Second, "call timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cc, err := dial(*dir, *addr)
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return helloCall(ctx, cc, info)
}

// dial connects to the agent at addr as the lab Provider of dir.
func dial(dir, addr string) (*grpc.ClientConn, error) {
	pdir := filepath.Join(dir, dirProvider)
	cert, err := tls.LoadX509KeyPair(filepath.Join(pdir, fileClient), filepath.Join(pdir, fileKey))
	if err != nil {
		return nil, fmt.Errorf("load the Provider identity: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(pdir, trust.FileHostCA)) // #nosec G304 -- a lab path given on the command line
	if err != nil {
		return nil, fmt.Errorf("read the host identity issuer: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificate in the host identity issuer file")
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		// The agent's certificate carries URI names only, so the default
		// host name check cannot apply; the chain is verified below.
		InsecureSkipVerify: true, // #nosec G402 -- verified in VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("the agent sent no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return fmt.Errorf("parse the agent's certificate: %w", err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
				return fmt.Errorf("verify the agent's certificate: %w", err)
			}
			return nil
		},
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return cc, nil
}

// helloCall calls Hello (or GetHostInfo) and prints the result line.
func helloCall(ctx context.Context, cc *grpc.ClientConn, info bool) error {
	var err error
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("draw a nonce: %w", err)
	}
	res := helloResult{}
	var blob *providerv1.SignedBlob
	if info {
		var resp *providerv1.HostInfo
		resp, err = providerv1.NewProviderClient(cc).GetHostInfo(ctx, &providerv1.GetHostInfoRequest{AnswerNonce: nonce, InventoryVersion: 1})
		blob = resp.GetHostAnswer()
	} else {
		var resp *agentv1.HelloResponse
		resp, err = agentv1.NewAgentServiceClient(cc).Hello(ctx, &agentv1.HelloRequest{AnswerNonce: nonce, CallerApiVersion: 1})
		blob = resp.GetHostAnswer()
	}
	switch {
	case err != nil:
		res.Error = err.Error()
	case blob != nil:
		a := &providerv1.HostAnswer{}
		if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
			return fmt.Errorf("decode the HostAnswer: %w", err)
		}
		res.Signed = true
		if res.Answer, err = protojson.Marshal(a); err != nil {
			return fmt.Errorf("encode the HostAnswer: %w", err)
		}
	}
	line, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode the result: %w", err)
	}
	fmt.Println(string(line))
	if res.Error != "" {
		return errors.New("the call failed")
	}
	return nil
}
