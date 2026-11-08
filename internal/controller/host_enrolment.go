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

// Per-host agent enrolment, manager side (development builds only).
//
// A Host whose Provider runs a per-host agent (spec.type cloudhypervisor)
// enrols its agent like this:
//
//  1. An administrator sets Host.spec.identity (immutable once set).
//  2. The HostEnrolmentReconciler draws an enrolment record for the host
//     (enrolment id, Provider UID, install id, mode identity), stores it in
//     the host's pins Secret and mirrors the exact URI subject alternative
//     names a request must carry into Host.status.agent.
//  3. The agent generates its key on the host and writes a CSR carrying
//     those URIs; an administrator submits it as a cert-manager
//     CertificateRequest in the enrolment namespace (vrtg host enroll).
//  4. The HostEnrolmentRequestReconciler checks the requester first (the
//     administrator group, then a SubjectAccessReview of the "enrol" verb on
//     the Host) without reading anything else, then the Host (exists, runs an
//     agent, has an identity, is not revoked, has a pending record, no
//     request in flight) and the request (hostidentity.Authorise). It records
//     the authorisation in the pins Secret first, then sets cert-manager's
//     Approved condition; any failure sets Denied with a reason.
//  5. Once a certificate is issued (by the cert-manager issuer, or returned
//     by an operator from an external PKI), the manager validates it against
//     the authorisation (hostidentity.ValidateIssued) and only then pins its
//     key.
//
// The pins Secrets live in the enrolment namespace, never in a Host's
// (tenant) namespace: a tenant's Secret write access is not enrolment
// authority. A pins Secret is named by a hash of the Host's namespace and
// name and bound to the Host UID and identity inside; it is deleted when the
// Host is deleted or goes away. The inventory render takes only the key pins
// from it: the CA bundle comes from the manager's configuration and the
// expected SAN is recomputed from the Host.
//
// Nothing here logs or records key material; CSRs and certificates are
// public but are kept out of logs and events too.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// Manager flags of host-agent enrolment. Only a manager built with the
// virtrigaud_dev build tag accepts them; a release build refuses to start
// when any is set.
const (
	// DevHostEnrolmentNamespaceFlagName names the enrolment namespace: the
	// issuer namespace, where enrolment CertificateRequests and the pins
	// Secrets live. Setting it enables host-agent enrolment.
	DevHostEnrolmentNamespaceFlagName = "dev-host-enrolment-namespace"
	// DevHostEnrolmentIssuerFlagName names the cert-manager issuer of the
	// host-agent subordinate CA, as kind/name or group/kind/name.
	DevHostEnrolmentIssuerFlagName = "dev-host-enrolment-issuer"
	// DevHostEnrolmentExternalPKIIssuerFlagName names the issuer reference
	// that marks a request for an external PKI, as group/kind/name with a
	// group other than cert-manager.io.
	DevHostEnrolmentExternalPKIIssuerFlagName = "dev-host-enrolment-external-pki-issuer"
	// DevHostEnrolmentAdminGroupFlagName names the group whose members may
	// request an enrolment.
	DevHostEnrolmentAdminGroupFlagName = "dev-host-enrolment-admin-group"
	// DevHostEnrolmentTrustDomainFlagName names the trust domain of host URI
	// identities.
	DevHostEnrolmentTrustDomainFlagName = "dev-host-enrolment-trust-domain"
	// DevHostEnrolmentCABundleFileFlagName names the PEM file of the
	// host-agent subordinate CA an issued certificate must chain to.
	DevHostEnrolmentCABundleFileFlagName = "dev-host-enrolment-ca-bundle-file"
)

// hostEnrolmentStateKey is the pins Secret's data key holding the
// enrolment state document.
const hostEnrolmentStateKey = "enrolment.json"

// hostEnrolmentSecretPrefix prefixes a pins Secret name.
const hostEnrolmentSecretPrefix = "virtrigaud-agent-pins-"

// hostEnrolmentComponent is the app.kubernetes.io/component label of a pins
// Secret.
const hostEnrolmentComponent = "host-agent-pins"

// maxRetiredEnrolmentIDs bounds the never-reuse list kept per Host (oldest
// dropped first; ids are 16 random bytes, so a dropped id is not redrawn in
// practice).
const maxRetiredEnrolmentIDs = 256

// Label keys and values of manager-owned objects.
const (
	k8sLabelComponent      = "app.kubernetes.io/component"
	k8sLabelManagedBy      = "app.kubernetes.io/managed-by"
	k8sManagedByVirtRigaud = "virtrigaud"
)

// Event reasons of host-agent enrolment, emitted on the Host.
const (
	eventReasonAgentEnrolmentDenied   = "AgentEnrolmentDenied"
	eventReasonAgentEnrolmentApproved = "AgentEnrolmentApproved"
	eventReasonAgentEnrolled          = "AgentEnrolled"
	eventReasonAgentCertRejected      = "AgentCertificateRejected"
	eventReasonAgentRevoked           = "AgentRevoked"
	eventReasonAgentRecordRetired     = "AgentEnrolmentRecordRetired"
)

// errForeignPinsSecret is returned when a Secret with a Host's pins Secret
// name exists but is not a manager-written pins Secret for that Host. The
// manager never reads it as pins nor overwrites it.
var errForeignPinsSecret = errors.New("a Secret with the host's pins Secret name exists and is not a VirtRigaud pins Secret for this Host")

// HostEnrolmentFlags are the raw values of the host-agent enrolment flags.
type HostEnrolmentFlags struct {
	// Namespace is --dev-host-enrolment-namespace.
	Namespace string
	// Issuer is --dev-host-enrolment-issuer.
	Issuer string
	// ExternalPKIIssuer is --dev-host-enrolment-external-pki-issuer.
	ExternalPKIIssuer string
	// AdminGroup is --dev-host-enrolment-admin-group.
	AdminGroup string
	// TrustDomain is --dev-host-enrolment-trust-domain.
	TrustDomain string
	// CABundleFile is --dev-host-enrolment-ca-bundle-file.
	CABundleFile string
}

// isSet reports whether any flag is set.
func (f HostEnrolmentFlags) isSet() bool {
	return f.Namespace != "" || f.Issuer != "" || f.ExternalPKIIssuer != "" || f.AdminGroup != "" ||
		f.TrustDomain != "" || f.CABundleFile != ""
}

// HostEnrolmentConfig configures host-agent enrolment.
type HostEnrolmentConfig struct {
	// Namespace is the enrolment namespace: where enrolment
	// CertificateRequests and the pins Secrets live. Only the manager may
	// write Secrets there.
	Namespace string
	// CertManagerIssuer is the cert-manager issuer of the subordinate CA, or
	// nil.
	CertManagerIssuer *certrequest.IssuerRef
	// ExternalPKIIssuer is the issuer reference of external-PKI requests, or
	// nil.
	ExternalPKIIssuer *certrequest.IssuerRef
	// AdminGroup is the group a requester must belong to.
	AdminGroup string
	// TrustDomain is the trust domain of host URI identities.
	TrustDomain string
	// CABundle is the PEM bundle of the subordinate CA. It is the only CA
	// bundle ever rendered into an inventory.
	CABundle []byte
	// InstallID is the 16-byte install identifier (from the development
	// operation signer).
	InstallID []byte
}

// ParseHostEnrolmentConfig turns the flags into a configuration. With no
// flag set it returns nil and no error (enrolment disabled). A release build
// refuses any flag. Otherwise the namespace, the admin group, the trust
// domain, the CA bundle, at least one issuer and the install id (a loaded
// development signer) are required, and an external-PKI reference must name
// an explicit group other than cert-manager.io (cert-manager's controllers
// would otherwise act on it).
func ParseHostEnrolmentConfig(f HostEnrolmentFlags, installID []byte) (*HostEnrolmentConfig, error) {
	if !f.isSet() {
		return nil, nil
	}
	if !devgate.Compiled {
		return nil, fmt.Errorf("the --dev-host-enrolment-* flags cannot be used with a release build: %w", devgate.ErrNotCompiled)
	}
	if f.Namespace == "" {
		return nil, fmt.Errorf("--%s is required to enable host-agent enrolment", DevHostEnrolmentNamespaceFlagName)
	}
	if f.AdminGroup == "" {
		return nil, fmt.Errorf("--%s is required", DevHostEnrolmentAdminGroupFlagName)
	}
	if err := hostidentity.ValidateTrustDomain(f.TrustDomain); err != nil {
		return nil, fmt.Errorf("--%s: %w", DevHostEnrolmentTrustDomainFlagName, err)
	}
	if len(installID) != hostidentity.IDLen {
		return nil, errors.New("host-agent enrolment needs the development operation signer (its install id)")
	}
	cfg := &HostEnrolmentConfig{
		Namespace:   f.Namespace,
		AdminGroup:  f.AdminGroup,
		TrustDomain: f.TrustDomain,
		InstallID:   append([]byte(nil), installID...),
	}
	if f.Issuer != "" {
		ref, err := certrequest.ParseIssuerRef(f.Issuer)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", DevHostEnrolmentIssuerFlagName, err)
		}
		cfg.CertManagerIssuer = &ref
	}
	if f.ExternalPKIIssuer != "" {
		if strings.Count(f.ExternalPKIIssuer, "/") != 2 {
			return nil, fmt.Errorf("--%s must be group/kind/name with an explicit group", DevHostEnrolmentExternalPKIIssuerFlagName)
		}
		ref, err := certrequest.ParseIssuerRef(f.ExternalPKIIssuer)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", DevHostEnrolmentExternalPKIIssuerFlagName, err)
		}
		if ref.Group == certrequest.Group {
			return nil, fmt.Errorf("--%s must not use the %s group: cert-manager's controllers act on it", DevHostEnrolmentExternalPKIIssuerFlagName, certrequest.Group)
		}
		cfg.ExternalPKIIssuer = &ref
	}
	if cfg.CertManagerIssuer == nil && cfg.ExternalPKIIssuer == nil {
		return nil, fmt.Errorf("--%s or --%s is required", DevHostEnrolmentIssuerFlagName, DevHostEnrolmentExternalPKIIssuerFlagName)
	}
	if f.CABundleFile == "" {
		return nil, fmt.Errorf("--%s is required", DevHostEnrolmentCABundleFileFlagName)
	}
	data, err := os.ReadFile(f.CABundleFile) // #nosec G304 -- an administrator-chosen development file
	if err != nil {
		return nil, fmt.Errorf("read --%s: %w", DevHostEnrolmentCABundleFileFlagName, err)
	}
	if _, err := hostidentity.RootPool(data); err != nil {
		return nil, fmt.Errorf("--%s: %w", DevHostEnrolmentCABundleFileFlagName, err)
	}
	cfg.CABundle = data
	return cfg, nil
}

// HostEnrolmentCacheOptions restricts the manager's CertificateRequest cache
// to the enrolment namespace, so the manager needs CertificateRequest access
// there only.
func HostEnrolmentCacheOptions(cfg *HostEnrolmentConfig) map[client.Object]cache.ByObject {
	if cfg == nil {
		return nil
	}
	return map[client.Object]cache.ByObject{
		certrequest.NewObject(): {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
	}
}

// HostCertificateIssuer is one path by which an authorised agent certificate
// request becomes a certificate. Both paths carry the request as a
// cert-manager CertificateRequest (so the API server authenticates the
// requester and the manager approves or denies it); they differ in who signs
// and in who reports the outcome.
type HostCertificateIssuer interface {
	// Name names the path ("certmanager" or "externalpki").
	Name() string
	// Handles reports whether a request targets this issuer.
	Handles(cr *unstructured.Unstructured) bool
	// Issued returns the issued chain (leaf first) once the request carries
	// one, or nil while it is pending.
	Issued(cr *unstructured.Unstructured) ([]byte, error)
	// Complete records on the request the outcome of validating its issued
	// certificate (validationErr nil: pinned). It reports whether it changed
	// the request.
	Complete(cr *unstructured.Unstructured, validationErr error, now time.Time) (bool, error)
}

// certManagerIssuer is the cert-manager path: the subordinate CA's
// cert-manager issuer signs an approved request and sets Ready=True.
type certManagerIssuer struct{ ref certrequest.IssuerRef }

// Name implements HostCertificateIssuer.
func (certManagerIssuer) Name() string { return "certmanager" }

// Handles implements HostCertificateIssuer.
func (i certManagerIssuer) Handles(cr *unstructured.Unstructured) bool {
	return certrequest.IssuerRefOf(cr) == i.ref
}

// Issued implements HostCertificateIssuer: the certificate counts only once
// cert-manager reports Ready=True.
func (certManagerIssuer) Issued(cr *unstructured.Unstructured) ([]byte, error) {
	if !certrequest.IsTrue(cr, certrequest.ConditionReady) {
		return nil, nil
	}
	return certrequest.Certificate(cr)
}

// Complete implements HostCertificateIssuer: cert-manager owns the request's
// Ready condition, so the outcome is recorded on the Host only.
func (certManagerIssuer) Complete(*unstructured.Unstructured, error, time.Time) (bool, error) {
	return false, nil
}

// externalPKIIssuer is the external-PKI path: an operator takes the
// authorised bundle to an enterprise CA and writes the returned certificate
// into the request's status.certificate (vrtg host enroll --certificate);
// the manager validates it and sets Ready.
type externalPKIIssuer struct{ ref certrequest.IssuerRef }

// Name implements HostCertificateIssuer.
func (externalPKIIssuer) Name() string { return "externalpki" }

// Handles implements HostCertificateIssuer.
func (i externalPKIIssuer) Handles(cr *unstructured.Unstructured) bool {
	return certrequest.IssuerRefOf(cr) == i.ref
}

// Issued implements HostCertificateIssuer: any returned certificate, until
// the manager has recorded an outcome (Ready with reason Issued or Failed;
// a Ready condition with any other reason is not the manager's and does not
// end the wait).
func (externalPKIIssuer) Issued(cr *unstructured.Unstructured) ([]byte, error) {
	if _, reason, found := certrequest.Condition(cr, certrequest.ConditionReady); found &&
		(reason == certrequest.ReasonIssued || reason == certrequest.ReasonFailed) {
		return nil, nil
	}
	return certrequest.Certificate(cr)
}

// Complete implements HostCertificateIssuer: Ready=True when pinned,
// Ready=False/Failed when refused.
func (externalPKIIssuer) Complete(cr *unstructured.Unstructured, validationErr error, now time.Time) (bool, error) {
	if validationErr != nil {
		return true, certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusFalse, certrequest.ReasonFailed,
			"the returned certificate does not match the authorisation: "+validationErr.Error(), now)
	}
	return true, certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued,
		"the returned certificate matches the authorisation and its key is pinned", now)
}

// issuers returns the configured issuer paths.
func (cfg *HostEnrolmentConfig) issuers() []HostCertificateIssuer {
	var out []HostCertificateIssuer
	if cfg.CertManagerIssuer != nil {
		out = append(out, certManagerIssuer{ref: *cfg.CertManagerIssuer})
	}
	if cfg.ExternalPKIIssuer != nil {
		out = append(out, externalPKIIssuer{ref: *cfg.ExternalPKIIssuer})
	}
	return out
}

// hostEnrolmentState is the authoritative enrolment state of one Host, kept
// in its pins Secret in the enrolment namespace. It holds public
// identifiers and key pins only.
type hostEnrolmentState struct {
	// HostUID and Identity bind the state to one Host object and identity: a
	// state for another UID or identity is discarded, never inherited.
	HostUID  string `json:"hostUID"`
	Identity string `json:"identity"`
	// Pending is the enrolment record drawn for the next enrolment.
	Pending *enrolmentRecord `json:"pending,omitempty"`
	// Authorised is the request authorised for the pending record.
	Authorised *authorisedRequest `json:"authorised,omitempty"`
	// Current is the enrolment whose key is pinned.
	Current *currentEnrolment `json:"current,omitempty"`
	// RetiredEnrolmentIDs are enrolment ids already used or abandoned for
	// the host; none is drawn again.
	RetiredEnrolmentIDs []string `json:"retiredEnrolmentIDs,omitempty"`
	// LastDenial is the last denied request of an authorised requester, for
	// the Host condition.
	LastDenial *requestOutcome `json:"lastDenial,omitempty"`
	// LastRejection is the last refused or abandoned issuance, for the Host
	// condition.
	LastRejection *requestOutcome `json:"lastRejection,omitempty"`
}

// enrolmentRecord is an enrolment record in hex.
type enrolmentRecord struct {
	EnrolmentID string `json:"enrolmentID"`
	ProviderUID string `json:"providerUID"`
	InstallID   string `json:"installID"`
}

// authorisedRequest is an authorised request.
type authorisedRequest struct {
	// Request is "namespace/name" of the CertificateRequest.
	Request string `json:"request"`
	// RequestUID is its UID.
	RequestUID string `json:"requestUID"`
	// Issuer is the issuer path name.
	Issuer string `json:"issuer"`
	// Record is the authorised enrolment record.
	Record enrolmentRecord `json:"record"`
	// KeyPin is the pin of the authorised key.
	KeyPin string `json:"keyPin"`
}

// currentEnrolment is a pinned enrolment. Only the key pins and the expiry
// are used by the render; the CA bundle and the expected SAN are never read
// from storage.
type currentEnrolment struct {
	Record   enrolmentRecord `json:"record"`
	KeyPins  []string        `json:"keyPins"`
	NotAfter metav1.Time     `json:"notAfter"`
}

// requestOutcome names a request and the reason of its outcome.
type requestOutcome struct {
	Request string `json:"request"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// toRecord converts to a hostidentity.Record.
func (e enrolmentRecord) toRecord() (hostidentity.Record, error) {
	eid, err := hostidentity.DecodeID(e.EnrolmentID)
	if err != nil {
		return hostidentity.Record{}, fmt.Errorf("enrolment id: %w", err)
	}
	iid, err := hostidentity.DecodeID(e.InstallID)
	if err != nil {
		return hostidentity.Record{}, fmt.Errorf("install id: %w", err)
	}
	rec := hostidentity.Record{EnrolmentID: eid, ProviderUID: e.ProviderUID, InstallID: iid, Mode: hostidentity.ModeIdentity}
	return rec, rec.Validate()
}

// retire adds enrolment ids to the never-reuse list, bounded by
// maxRetiredEnrolmentIDs.
func (st *hostEnrolmentState) retire(ids ...string) {
	for _, id := range ids {
		if id != "" && !slices.Contains(st.RetiredEnrolmentIDs, id) {
			st.RetiredEnrolmentIDs = append(st.RetiredEnrolmentIDs, id)
		}
	}
	if n := len(st.RetiredEnrolmentIDs); n > maxRetiredEnrolmentIDs {
		st.RetiredEnrolmentIDs = slices.Clone(st.RetiredEnrolmentIDs[n-maxRetiredEnrolmentIDs:])
	}
}

// retireOpenRecords retires the pending and authorised records and clears
// them (the next record is drawn fresh).
func (st *hostEnrolmentState) retireOpenRecords() {
	if st.Pending != nil {
		st.retire(st.Pending.EnrolmentID)
	}
	if st.Authorised != nil {
		st.retire(st.Authorised.Record.EnrolmentID)
	}
	st.Pending, st.Authorised = nil, nil
}

// hostEnrolmentSecretName returns the name of the pins Secret of the Host
// namespace/name: a fixed prefix and a hash, so it is bounded whatever the
// Host's name and never collides with another Host's.
func hostEnrolmentSecretName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return hostEnrolmentSecretPrefix + hex.EncodeToString(sum[:16])
}

// hostKey is "namespace/name" of a Host.
func hostKey(namespace, name string) string { return namespace + "/" + name }

// loadedEnrolment is a Host's state and the Secret it was read from (nil
// when absent).
type loadedEnrolment struct {
	state  hostEnrolmentState
	secret *corev1.Secret
	// reset reports that a stored state for another Host object or
	// identity was discarded.
	reset bool
}

// loadHostEnrolment reads a Host's pins Secret from the enrolment
// namespace. A missing Secret yields a fresh state. A state that names
// another UID or identity (a replaced host) yields a fresh state with reset
// set: the old pins are never inherited. A Secret that is not a VirtRigaud
// pins Secret for this Host is refused (errForeignPinsSecret).
func loadHostEnrolment(ctx context.Context, c client.Reader, cfg *HostEnrolmentConfig, host *infravirtrigaudiov1beta1.Host) (loadedEnrolment, error) {
	fresh := hostEnrolmentState{HostUID: string(host.UID), Identity: host.Spec.Identity}
	sec := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: hostEnrolmentSecretName(host.Namespace, host.Name)}, sec)
	if apierrors.IsNotFound(err) {
		return loadedEnrolment{state: fresh}, nil
	}
	if err != nil {
		return loadedEnrolment{}, fmt.Errorf("get pins Secret of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	if !isPinsSecretFor(sec, host.Namespace, host.Name) {
		return loadedEnrolment{}, fmt.Errorf("Host %s/%s: %w", host.Namespace, host.Name, errForeignPinsSecret)
	}
	var st hostEnrolmentState
	if err := json.Unmarshal(sec.Data[hostEnrolmentStateKey], &st); err != nil ||
		st.HostUID != string(host.UID) || st.Identity != host.Spec.Identity {
		return loadedEnrolment{state: fresh, secret: sec, reset: true}, nil
	}
	return loadedEnrolment{state: st, secret: sec}, nil
}

// isPinsSecretFor reports whether a Secret is the manager's pins Secret for
// the Host namespace/name (labels and the Host annotation).
func isPinsSecretFor(sec *corev1.Secret, namespace, name string) bool {
	return sec.Labels[k8sLabelComponent] == hostEnrolmentComponent &&
		sec.Labels[k8sLabelManagedBy] == k8sManagedByVirtRigaud &&
		sec.Annotations[certrequest.AnnotationHost] == hostKey(namespace, name)
}

// saveHostEnrolment writes a Host's state to its pins Secret in the
// enrolment namespace, creating it when absent. An update carries the read
// resourceVersion, so a concurrent writer makes it fail with a conflict
// instead of being overwritten. No owner reference: the Secret is in
// another namespace than the Host, and is deleted by the Host reconciler.
func saveHostEnrolment(ctx context.Context, c client.Client, cfg *HostEnrolmentConfig, host *infravirtrigaudiov1beta1.Host, l *loadedEnrolment) error {
	data, err := json.Marshal(l.state)
	if err != nil {
		return fmt.Errorf("marshal enrolment state of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	sec := l.secret
	create := sec == nil
	if create {
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: hostEnrolmentSecretName(host.Namespace, host.Name)}}
	} else {
		sec = sec.DeepCopy()
	}
	sec.Labels = map[string]string{k8sLabelComponent: hostEnrolmentComponent, k8sLabelManagedBy: k8sManagedByVirtRigaud}
	sec.Annotations = map[string]string{certrequest.AnnotationHost: hostKey(host.Namespace, host.Name)}
	sec.OwnerReferences = nil
	sec.Type = corev1.SecretTypeOpaque
	sec.Data = map[string][]byte{hostEnrolmentStateKey: data}
	if create {
		err = c.Create(ctx, sec)
	} else {
		err = c.Update(ctx, sec)
	}
	if err != nil {
		return fmt.Errorf("write pins Secret of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	l.secret = sec
	l.reset = false
	return nil
}

// deleteHostEnrolment deletes the pins Secret of the Host namespace/name,
// if it is the manager's and its stored Host UID is ownerUID (empty: any
// stored UID). The Secret is read through reader (pass a live reader, so a
// stale cache never decides a deletion) and deleted with UID and
// resourceVersion preconditions, so a Secret rewritten in between is kept.
func deleteHostEnrolment(ctx context.Context, c client.Client, reader client.Reader, cfg *HostEnrolmentConfig, namespace, name string, ownerUID types.UID) error {
	sec := &corev1.Secret{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: hostEnrolmentSecretName(namespace, name)}, sec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get pins Secret of Host %s/%s: %w", namespace, name, err)
	}
	if !isPinsSecretFor(sec, namespace, name) {
		return nil
	}
	if ownerUID != "" {
		var st hostEnrolmentState
		if err := json.Unmarshal(sec.Data[hostEnrolmentStateKey], &st); err == nil && st.HostUID != string(ownerUID) {
			return nil
		}
	}
	rv := sec.ResourceVersion
	if err := c.Delete(ctx, sec, client.Preconditions{UID: &sec.UID, ResourceVersion: &rv}); client.IgnoreNotFound(err) != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("delete pins Secret of Host %s/%s: %w", namespace, name, err)
	}
	return nil
}

// hostRunsAgent returns the Host's Provider when it runs a per-host agent
// (spec.type cloudhypervisor, topology cluster, same namespace).
func hostRunsAgent(ctx context.Context, c client.Reader, host *infravirtrigaudiov1beta1.Host) (*infravirtrigaudiov1beta1.Provider, bool, error) {
	p := &infravirtrigaudiov1beta1.Provider{}
	err := c.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.ProviderRef.Name}, p)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get Provider of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	return p, providerRunsHostAgents(p), nil
}

// providerRunsHostAgents reports whether a Provider runs a per-host agent.
func providerRunsHostAgents(p *infravirtrigaudiov1beta1.Provider) bool {
	return p.Spec.Type == infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor && isClusterTopology(p)
}

// enrolmentRequestLive reports whether an authorised request still exists
// (same UID) and has not failed.
func enrolmentRequestLive(ctx context.Context, c client.Reader, cfg *HostEnrolmentConfig, a *authorisedRequest) (bool, error) {
	other := certrequest.NewObject()
	_, name, _ := strings.Cut(a.Request, "/")
	err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, other)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get authorised request %s: %w", a.Request, err)
	}
	return string(other.GetUID()) == a.RequestUID && !certrequest.Failed(other), nil
}

// HostEnrolmentReconciler draws a Host's enrolment record, keeps its pins
// Secret bound to the Host object and identity, retires the enrolment of a
// revoked Host, redraws the record after an abandoned issuance, deletes the
// pins Secret of a deleted Host, and mirrors the Secret into
// Host.status.agent and the AgentEnrolled condition. Development builds
// only: the manager registers it only with a HostEnrolmentConfig.
type HostEnrolmentReconciler struct {
	client.Client
	// APIReader confirms a Host is gone before its pins Secret is deleted
	// (nil: the Client).
	APIReader client.Reader
	// Config is the enrolment configuration.
	Config *HostEnrolmentConfig
	// Recorder emits events on Hosts (nil: none).
	Recorder record.EventRecorder
	// Rand draws enrolment ids (nil: crypto/rand).
	Rand io.Reader
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch

// Reconcile implements reconcile.Reconciler.
func (r *HostEnrolmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.forgetGoneHost(ctx, req.NamespacedName)
	}
	if !host.DeletionTimestamp.IsZero() {
		// A deleting Host is never enrolled again; its pins go first.
		return ctrl.Result{}, deleteHostEnrolment(ctx, r.Client, r.liveReader(), r.Config, host.Namespace, host.Name, host.UID)
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil || !agent {
		return ctrl.Result{}, err
	}
	if host.Spec.Identity == "" {
		return ctrl.Result{}, r.mirror(ctx, host, nil)
	}
	l, err := loadHostEnrolment(ctx, r.Client, r.Config, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	changed := l.reset
	if l.reset {
		log.FromContext(ctx).Info("Discarded the enrolment state of a previous Host object or identity; no pin is inherited",
			"host", req.String())
	}
	st := &l.state
	switch {
	case host.Spec.Revoked:
		if st.Current != nil || st.Pending != nil || st.Authorised != nil {
			if st.Current != nil {
				st.retire(st.Current.Record.EnrolmentID)
			}
			st.retireOpenRecords()
			st.Current = nil
			changed = true
			log.FromContext(ctx).Info("Revoked a host agent: its enrolment is retired and its pins removed", "host", req.String())
			r.event(host, corev1.EventTypeWarning, eventReasonAgentRevoked, "the agent enrolment is retired and its key pins removed")
		}
	default:
		if a := st.Authorised; a != nil {
			live, err := enrolmentRequestLive(ctx, r.Client, r.Config, a)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !live {
				st.LastRejection = &requestOutcome{Request: a.Request, Reason: infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected,
					Message: "the authorised request was deleted or failed before a certificate was pinned"}
				st.retireOpenRecords()
				changed = true
				r.event(host, corev1.EventTypeNormal, eventReasonAgentRecordRetired, "the enrolment record of abandoned request "+a.Request+" is retired")
			}
		}
		if st.Current == nil {
			if p := st.Pending; p == nil || p.ProviderUID != string(provider.UID) || p.InstallID != hex.EncodeToString(r.Config.InstallID) {
				if p != nil {
					st.retireOpenRecords()
				}
				id, err := r.drawEnrolmentID(st.RetiredEnrolmentIDs)
				if err != nil {
					return ctrl.Result{}, err
				}
				st.Pending = &enrolmentRecord{EnrolmentID: id, ProviderUID: string(provider.UID), InstallID: hex.EncodeToString(r.Config.InstallID)}
				changed = true
			}
		}
	}
	if changed {
		if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
			return ctrl.Result{}, err
		}
	}
	res := ctrl.Result{}
	if c := st.Current; c != nil && !host.Spec.Revoked {
		// Re-mirror at expiry so the condition and the render follow it.
		if d := c.NotAfter.Sub(r.now()); d > 0 {
			res.RequeueAfter = d + time.Second
		}
	}
	return res, r.mirror(ctx, host, st)
}

// forgetGoneHost deletes the pins Secret of a Host that no longer exists.
// Everything is re-read through the live reader: the Secret, its stored Host
// UID, and the Host; a Host that exists (under any UID) keeps the Secret, and
// the delete carries UID and resourceVersion preconditions.
func (r *HostEnrolmentReconciler) forgetGoneHost(ctx context.Context, key types.NamespacedName) error {
	reader := r.liveReader()
	if err := reader.Get(ctx, key, &infravirtrigaudiov1beta1.Host{}); !apierrors.IsNotFound(err) {
		return client.IgnoreNotFound(err)
	}
	return deleteHostEnrolment(ctx, r.Client, reader, r.Config, key.Namespace, key.Name, "")
}

// liveReader returns the APIReader, or the Client when none is set.
func (r *HostEnrolmentReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// now returns the current time.
func (r *HostEnrolmentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// event emits an event on the Host when a recorder is set.
func (r *HostEnrolmentReconciler) event(host *infravirtrigaudiov1beta1.Host, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(host, typ, reason, msg)
	}
}

// drawEnrolmentID draws an enrolment id not on the retired list.
func (r *HostEnrolmentReconciler) drawEnrolmentID(retired []string) (string, error) {
	src := r.Rand
	if src == nil {
		src = rand.Reader
	}
	for range 8 {
		id, err := hostidentity.NewEnrolmentID(src)
		if err != nil {
			return "", err
		}
		if s := hex.EncodeToString(id); !slices.Contains(retired, s) {
			return s, nil
		}
	}
	return "", errors.New("draw enrolment id: every draw was a retired id")
}

// mirror writes Host.status.agent and the AgentEnrolled condition from the
// state (nil: no state, identity missing). The patch carries the read
// resourceVersion, so it never overwrites a concurrent status writer.
func (r *HostEnrolmentReconciler) mirror(ctx context.Context, host *infravirtrigaudiov1beta1.Host, st *hostEnrolmentState) error {
	base := host.DeepCopy()
	cond := metav1.Condition{Type: infravirtrigaudiov1beta1.HostConditionAgentEnrolled, Status: metav1.ConditionFalse, ObservedGeneration: host.Generation}
	var agent *infravirtrigaudiov1beta1.HostAgentStatus
	switch {
	case host.Spec.Identity == "" || st == nil:
		cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentIdentityMissing, "spec.identity is not set; no enrolment is authorised"
	default:
		agent = &infravirtrigaudiov1beta1.HostAgentStatus{Mode: infravirtrigaudiov1beta1.HostAgentModeIdentity}
		hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
		if err != nil {
			return fmt.Errorf("host URI of Host %s/%s: %w", host.Namespace, host.Name, err)
		}
		if p := st.Pending; p != nil {
			agent.PendingEnrolmentID = p.EnrolmentID
			if rec, err := p.toRecord(); err == nil {
				agent.RequestURIs = hostidentity.RequestURIs(hostURI, rec)
			}
		}
		if c := st.Current; c != nil {
			agent.EnrolmentID = c.Record.EnrolmentID
			agent.KeyPins = append([]string(nil), c.KeyPins...)
			na := c.NotAfter
			agent.CertificateExpiry = &na
		}
		switch {
		case host.Spec.Revoked:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentRevoked, "spec.revoked is true; the enrolment is retired and no enrolment is authorised"
		case st.Current != nil && !st.Current.NotAfter.After(r.now()):
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentCertificateExpired, "the pinned agent certificate has expired"
		case st.Current != nil:
			cond.Status = metav1.ConditionTrue
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentEnrolled, "the agent certificate is validated and its key is pinned"
		case st.Authorised != nil:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentAwaitingCertificate,
				fmt.Sprintf("certificate request %s is authorised; waiting for the issued certificate", st.Authorised.Request)
		case st.LastRejection != nil:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected,
				fmt.Sprintf("the issuance for request %s was refused or abandoned (%s); a new record is drawn", st.LastRejection.Request, st.LastRejection.Message)
		case st.LastDenial != nil:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentRequestDenied,
				fmt.Sprintf("certificate request %s was denied: %s", st.LastDenial.Request, st.LastDenial.Reason)
		default:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentAwaitingRequest,
				"waiting for a certificate request carrying status.agent.requestURIs"
		}
	}
	host.Status.Agent = agent
	meta.SetStatusCondition(&host.Status.Conditions, cond)
	if equality.Semantic.DeepEqual(base.Status, host.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("mirror enrolment status of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	return nil
}

// hostRequestFromAnnotation maps an object carrying the Host annotation
// (a pins Secret or an enrolment request in the enrolment namespace) to its
// Host.
func (r *HostEnrolmentReconciler) hostRequestFromAnnotation(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Config.Namespace {
		return nil
	}
	ns, name, ok := strings.Cut(obj.GetAnnotations()[certrequest.AnnotationHost], "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// SetupWithManager registers the reconciler: Hosts, the pins Secrets in the
// enrolment namespace (so a Secret change is mirrored at once, and every
// pins Secret is visited at start-up, which deletes those of Hosts gone
// while the manager was down), and enrolment requests (so an abandoned
// request's record is retired).
func (r *HostEnrolmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inEnrolmentNS := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == r.Config.Namespace })
	isPins := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[k8sLabelComponent] == hostEnrolmentComponent
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.Host{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.hostRequestFromAnnotation),
			builder.WithPredicates(inEnrolmentNS, isPins)).
		Watches(certrequest.NewObject(), handler.EnqueueRequestsFromMapFunc(r.hostRequestFromAnnotation),
			builder.WithPredicates(inEnrolmentNS)).
		Named("host-enrolment").
		Complete(r)
}

// resolveHostAgentTLS returns the agentTLS credentials of a Host whose
// Provider runs a per-host agent, or a non-secret skip reason. Only the key
// pins come from the pins Secret (in the enrolment namespace); the CA bundle
// is the manager's configured subordinate CA and the expected SAN is
// recomputed from the Host. Release builds, and managers without enrolment,
// render no agent credentials.
func resolveHostAgentTLS(ctx context.Context, c client.Reader, cfg *HostEnrolmentConfig, host *infravirtrigaudiov1beta1.Host, now time.Time) (hostsecret.Credentials, string) {
	switch {
	case !devgate.Compiled:
		return hostsecret.Credentials{}, "per-host agent hosts are rendered only by development builds"
	case cfg == nil:
		return hostsecret.Credentials{}, "per-host agent enrolment is not enabled in this manager"
	case host.Spec.Revoked:
		return hostsecret.Credentials{}, "host is revoked"
	case host.Spec.Identity == "":
		return hostsecret.Credentials{}, "host has no spec.identity"
	}
	l, err := loadHostEnrolment(ctx, c, cfg, host)
	if err != nil {
		if errors.Is(err, errForeignPinsSecret) {
			return hostsecret.Credentials{}, "host pins Secret is not a VirtRigaud pins Secret"
		}
		return hostsecret.Credentials{}, "host pins Secret unreadable"
	}
	if l.reset || l.state.Current == nil {
		return hostsecret.Credentials{}, "host agent is not enrolled"
	}
	if !l.state.Current.NotAfter.After(now) {
		return hostsecret.Credentials{}, "host agent certificate expired"
	}
	expected, err := hostidentity.HostURI(cfg.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return hostsecret.Credentials{}, "host has no valid URI identity"
	}
	creds := hostsecret.Credentials{AgentTLS: &hostsecret.AgentTLS{
		CABundle: append([]byte(nil), cfg.CABundle...), ExpectedSAN: expected, KeyPins: append([]string(nil), l.state.Current.KeyPins...),
	}}
	if err := creds.Validate(); err != nil {
		return hostsecret.Credentials{}, "host pins are malformed"
	}
	return creds, ""
}

// Compile-time assertions.
var (
	_ HostCertificateIssuer = certManagerIssuer{}
	_ HostCertificateIssuer = externalPKIIssuer{}
)
