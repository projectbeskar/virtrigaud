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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// Flag names of the host commands.
const (
	flagHost            = "host"
	flagCSR             = "csr"
	flagIssuerNamespace = "issuer-namespace"
	flagIssuer          = "issuer"
	flagDuration        = "duration"
	flagManifestOut     = "manifest-out"
	flagRequest         = "request"
	flagBundleOut       = "bundle-out"
	flagCertificate     = "certificate"
)

// maxEnrolFileSize bounds the files the host commands read (a CSR, a
// certificate chain).
const maxEnrolFileSize = 64 << 10

// hostEnrollOptions holds the values of vrtg host enroll's flags.
type hostEnrollOptions struct {
	host            string
	csrFile         string
	issuerNamespace string
	issuer          string
	duration        time.Duration
	manifestOut     string
	request         string
	bundleOut       string
	certificateFile string
}

// newHostCmd builds the host command tree: enroll and revoke, for hosts of a
// Provider that runs a per-host agent (development builds only).
func (o *options) newHostCmd() *cobra.Command {
	hostCmd := &cobra.Command{
		Use:     "host",
		Aliases: []string{"hosts"},
		Short:   "Enrol or revoke per-host agents (development builds only)",
	}

	eo := &hostEnrollOptions{}
	enroll := &cobra.Command{
		Use:   "enroll --host <name> --csr <file>",
		Short: "Submit a per-host agent's certificate signing request for enrolment",
		Long: `Submit the certificate signing request a per-host agent wrote (copy the file
from the host; the host needs no access to the cluster) as a cert-manager
CertificateRequest for the host-agent issuer. The manager approves it only if
every enrolment rule holds; your user must be in the enrolment administrator
group.

With --manifest-out the request is written to a file instead of being created,
so it can be reviewed and created later (kubectl create -f) from another
machine: no cluster access is needed.

External PKI: once the manager approved a request (--request <name>), write
the authorised bundle with --bundle-out, submit it to the enterprise CA, and
return the certificate with --certificate <file>. The manager pins the key
only after checking the certificate against the authorisation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return o.hostEnroll(cmd, eo) },
	}
	f := enroll.Flags()
	f.StringVar(&eo.host, flagHost, "", "Host name (in --namespace)")
	f.StringVar(&eo.csrFile, flagCSR, "", "PEM certificate signing request file written by the per-host agent")
	f.StringVar(&eo.issuerNamespace, flagIssuerNamespace, "", "Namespace of the enrolment CertificateRequests (the host-agent issuer's)")
	f.StringVar(&eo.issuer, flagIssuer, "", "Issuer reference, kind/name or group/kind/name (the cert-manager host-agent issuer, or the external-PKI reference)")
	f.DurationVar(&eo.duration, flagDuration, hostidentity.MaxLifetime, "Requested certificate lifetime (at most 720h)")
	f.StringVar(&eo.manifestOut, flagManifestOut, "", "Write the CertificateRequest to this file instead of creating it (offline)")
	f.StringVar(&eo.request, flagRequest, "", "External PKI: name of the approved CertificateRequest")
	f.StringVar(&eo.bundleOut, flagBundleOut, "", "External PKI: write the authorised bundle (CSR and issuance policy) to this file")
	f.StringVar(&eo.certificateFile, flagCertificate, "", "External PKI: PEM certificate (leaf first) returned by the enterprise CA")
	_ = enroll.MarkFlagRequired(flagHost)

	revoke := &cobra.Command{
		Use:   "revoke --host <name>",
		Short: "Revoke a per-host agent's identity (sets spec.revoked)",
		Long: `Set spec.revoked on the Host. The manager then never approves a certificate
request for it, and the host is no longer rendered into its Provider's
inventory with agent credentials.`,
		Args: cobra.NoArgs,
	}
	var revokeHost string
	revoke.Flags().StringVar(&revokeHost, flagHost, "", "Host name (in --namespace)")
	_ = revoke.MarkFlagRequired(flagHost)
	revoke.RunE = func(cmd *cobra.Command, _ []string) error { return o.hostRevoke(cmd, revokeHost) }

	hostCmd.AddCommand(enroll, revoke)
	return hostCmd
}

// hostEnroll runs vrtg host enroll in one of its three modes.
func (o *options) hostEnroll(cmd *cobra.Command, eo *hostEnrollOptions) error {
	switch {
	case eo.request != "" && (eo.bundleOut != "") == (eo.certificateFile != ""):
		return fmt.Errorf("with --%s, give exactly one of --%s and --%s", flagRequest, flagBundleOut, flagCertificate)
	case eo.request != "" && eo.csrFile != "":
		return fmt.Errorf("--%s and --%s cannot be combined", flagRequest, flagCSR)
	case eo.request == "" && (eo.bundleOut != "" || eo.certificateFile != ""):
		return fmt.Errorf("--%s and --%s need --%s", flagBundleOut, flagCertificate, flagRequest)
	case eo.issuerNamespace == "":
		return fmt.Errorf("--%s is required", flagIssuerNamespace)
	case eo.request != "" && eo.bundleOut != "":
		return o.hostEnrollBundle(cmd, eo)
	case eo.request != "":
		return o.hostEnrollCertificate(cmd, eo)
	}
	return o.hostEnrollSubmit(cmd, eo)
}

// readBounded reads a regular file of at most maxEnrolFileSize bytes. The
// size is checked on the opened file and the read is bounded, so a FIFO,
// a device or a file that grows cannot make it read without limit.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a file the administrator names
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEnrolFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEnrolFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxEnrolFileSize)
	}
	return data, nil
}

// writeFileSafely writes data to path with mode 0600 through a temporary file
// in the same directory and a rename, so it never follows a symbolic link at
// path and never keeps the mode of a file already there. A symbolic link at
// path is refused.
func writeFileSafely(path string, data []byte) error {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; refusing to write through it", path)
	} else if err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// hostEnrollSubmit checks a CSR file and creates (or writes) the
// CertificateRequest.
func (o *options) hostEnrollSubmit(cmd *cobra.Command, eo *hostEnrollOptions) error {
	if eo.csrFile == "" {
		return fmt.Errorf("--%s is required", flagCSR)
	}
	if eo.issuer == "" {
		return fmt.Errorf("--%s is required", flagIssuer)
	}
	ref, err := certrequest.ParseIssuerRef(eo.issuer)
	if err != nil {
		return err
	}
	if eo.duration <= 0 || eo.duration > hostidentity.MaxLifetime {
		return fmt.Errorf("--%s must be positive and at most %s", flagDuration, hostidentity.MaxLifetime)
	}
	csrPEM, err := readBounded(eo.csrFile)
	if err != nil {
		return fmt.Errorf("read --%s: %w", flagCSR, err)
	}
	// Local checks, so a bad file fails here rather than as a denial: one
	// PEM CSR whose signature verifies (proof of possession).
	csr, err := hostidentity.ParseCSR(csrPEM)
	if err != nil {
		return fmt.Errorf("--%s: %w", flagCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return fmt.Errorf("--%s: the request's signature does not verify with its public key", flagCSR)
	}
	cr := certrequest.New(certrequest.Spec{
		Namespace: eo.issuerNamespace, GenerateName: "host-enrol-", HostNamespace: o.namespace, HostName: eo.host,
		IssuerRef: ref, CSRPEM: csrPEM, Duration: eo.duration,
	})

	if eo.manifestOut != "" {
		data, err := yaml.Marshal(cr.Object)
		if err != nil {
			return fmt.Errorf("render CertificateRequest: %w", err)
		}
		if err := writeFileSafely(eo.manifestOut, data); err != nil {
			return fmt.Errorf("write --%s: %w", flagManifestOut, err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Wrote the enrolment request for Host %s/%s to %s (create it with kubectl create -f)\n",
			o.namespace, eo.host, eo.manifestOut)
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()
	c, err := o.getClient()
	if err != nil {
		return err
	}
	host := &infrav1beta1.Host{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.namespace, Name: eo.host}, host); err != nil {
		return fmt.Errorf("get Host %s/%s: %w", o.namespace, eo.host, err)
	}
	if host.Status.Agent == nil || len(host.Status.Agent.RequestURIs) == 0 {
		return fmt.Errorf("Host %s/%s has no pending enrolment (status.agent.requestURIs is empty)", o.namespace, eo.host)
	}
	got := make([]string, 0, len(csr.URIs))
	for _, u := range csr.URIs {
		got = append(got, u.String())
	}
	if !sameSet(got, host.Status.Agent.RequestURIs) {
		return fmt.Errorf("--%s does not carry exactly the URIs in Host %s/%s status.agent.requestURIs", flagCSR, o.namespace, eo.host)
	}
	if err := c.Create(ctx, cr); err != nil {
		return fmt.Errorf("create CertificateRequest in %s: %w", eo.issuerNamespace, err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created CertificateRequest %s/%s for Host %s/%s\n", cr.GetNamespace(), cr.GetName(), o.namespace, eo.host)
	return err
}

// sameSet reports whether a and b hold the same strings, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

// approvedRequest fetches a request for the Host that the manager approved.
func (o *options) approvedRequest(cmd *cobra.Command, c client.Client, eo *hostEnrollOptions) (*unstructured.Unstructured, error) {
	ctx, cancel := o.newContext(cmd)
	defer cancel()
	cr := certrequest.NewObject()
	if err := c.Get(ctx, types.NamespacedName{Namespace: eo.issuerNamespace, Name: eo.request}, cr); err != nil {
		return nil, fmt.Errorf("get CertificateRequest %s/%s: %w", eo.issuerNamespace, eo.request, err)
	}
	ns, name, ok := certrequest.HostOf(cr)
	if !certrequest.IsEnrolment(cr) || !ok || ns != o.namespace || name != eo.host {
		return nil, fmt.Errorf("CertificateRequest %s/%s is not an enrolment request for Host %s/%s", eo.issuerNamespace, eo.request, o.namespace, eo.host)
	}
	if !certrequest.IsTrue(cr, certrequest.ConditionApproved) || certrequest.IsTrue(cr, certrequest.ConditionDenied) {
		return nil, fmt.Errorf("CertificateRequest %s/%s is not approved", eo.issuerNamespace, eo.request)
	}
	return cr, nil
}

// hostEnrollBundle writes the authorised bundle of an approved request.
func (o *options) hostEnrollBundle(cmd *cobra.Command, eo *hostEnrollOptions) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}
	cr, err := o.approvedRequest(cmd, c, eo)
	if err != nil {
		return err
	}
	b, err := buildBundle(cr)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("render bundle: %w", err)
	}
	if err := writeFileSafely(eo.bundleOut, append(data, '\n')); err != nil {
		return fmt.Errorf("write --%s: %w", flagBundleOut, err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Wrote the authorised bundle of %s/%s to %s\n", eo.issuerNamespace, eo.request, eo.bundleOut)
	return err
}

// buildBundle builds the external-PKI bundle of an approved request: its CSR
// and the issuance policy the certificate must meet.
func buildBundle(cr *unstructured.Unstructured) (certrequest.Bundle, error) {
	csrPEM, err := certrequest.CSR(cr)
	if err != nil {
		return certrequest.Bundle{}, err
	}
	csr, err := hostidentity.ParseCSR(csrPEM)
	if err != nil {
		return certrequest.Bundle{}, err
	}
	in, err := certrequest.RequestOf(cr)
	if err != nil {
		return certrequest.Bundle{}, err
	}
	lifetime := hostidentity.MaxLifetime
	if in.Duration > 0 && in.Duration < lifetime {
		lifetime = in.Duration
	}
	uris := make([]string, 0, len(csr.URIs))
	for _, u := range csr.URIs {
		uris = append(uris, u.String())
	}
	ns, name, _ := certrequest.HostOf(cr)
	return certrequest.Bundle{
		Host:               ns + "/" + name,
		Request:            cr.GetNamespace() + "/" + cr.GetName(),
		CSR:                string(csrPEM),
		SubjectAltNameURIs: uris,
		ExtendedKeyUsages:  []string{hostidentity.UsageClientAuth, hostidentity.UsageServerAuth},
		MaxLifetime:        lifetime.String(),
		CA:                 false,
	}, nil
}

// hostEnrollCertificate returns an external-PKI certificate into an approved
// request's status.certificate.
func (o *options) hostEnrollCertificate(cmd *cobra.Command, eo *hostEnrollOptions) error {
	chain, err := readBounded(eo.certificateFile)
	if err != nil {
		return fmt.Errorf("read --%s: %w", flagCertificate, err)
	}
	if _, err := hostidentity.ParseCertificates(chain); err != nil {
		return fmt.Errorf("--%s: %w", flagCertificate, err)
	}
	c, err := o.getClient()
	if err != nil {
		return err
	}
	cr, err := o.approvedRequest(cmd, c, eo)
	if err != nil {
		return err
	}
	if existing, err := certrequest.Certificate(cr); err != nil || existing != nil {
		return errors.New("the request already carries a certificate")
	}
	if err := certrequest.SetCertificate(cr, chain, nil); err != nil {
		return err
	}
	ctx, cancel := o.newContext(cmd)
	defer cancel()
	if err := c.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("return the certificate to %s/%s: %w", eo.issuerNamespace, eo.request, err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Returned the certificate to %s/%s; the manager pins its key only if it matches the authorisation\n",
		eo.issuerNamespace, eo.request)
	return err
}

// hostRevoke sets spec.revoked on a Host.
func (o *options) hostRevoke(cmd *cobra.Command, name string) error {
	ctx, cancel := o.newContext(cmd)
	defer cancel()
	c, err := o.getClient()
	if err != nil {
		return err
	}
	host := &infrav1beta1.Host{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.namespace, Name: name}, host); err != nil {
		return fmt.Errorf("get Host %s/%s: %w", o.namespace, name, err)
	}
	if !host.Spec.Revoked {
		base := host.DeepCopy()
		host.Spec.Revoked = true
		if err := c.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("revoke Host %s/%s: %w", o.namespace, name, err)
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Host %s/%s is revoked\n", o.namespace, name)
	return err
}
