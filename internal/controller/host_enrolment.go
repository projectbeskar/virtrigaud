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
//     the host's manager-owned pins Secret and mirrors the exact URI subject
//     alternative names a request must carry into Host.status.agent.
//  3. The agent generates its key on the host and writes a CSR carrying
//     those URIs; an administrator submits it as a cert-manager
//     CertificateRequest in the enrolment namespace (vrtg host enroll).
//  4. The HostEnrolmentRequestReconciler authorises the request against the
//     rules in hostidentity.Authorise plus the Host checks here (the Host
//     exists, runs an agent, has an identity, is not revoked, has a pending
//     record and no other request in flight). It records the authorisation in
//     the pins Secret first, then sets cert-manager's Approved condition; any
//     failure sets Denied with a reason. cert-manager's default approver must
//     be disabled for the enrolment issuer, or it would approve first.
//  5. Once a certificate is issued (by the cert-manager issuer, or returned
//     by an operator from an external PKI), the manager validates it against
//     the authorisation (hostidentity.ValidateIssued) and only then pins its
//     key in the Secret. The Secret is authoritative; Host.status.agent only
//     mirrors it and is rewritten from it when lost.
//
// Nothing here logs or records key material; CSRs and certificates are
// public but are kept out of logs and events too.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
	// DevHostEnrolmentNamespaceFlagName names the namespace enrolment
	// CertificateRequests are created in (the issuer namespace). Setting it
	// enables host-agent enrolment.
	DevHostEnrolmentNamespaceFlagName = "dev-host-enrolment-namespace"
	// DevHostEnrolmentIssuerFlagName names the cert-manager issuer of the
	// host-agent subordinate CA, as kind/name or group/kind/name.
	DevHostEnrolmentIssuerFlagName = "dev-host-enrolment-issuer"
	// DevHostEnrolmentExternalPKIIssuerFlagName names the issuer reference
	// that marks a request for an external PKI, as group/kind/name.
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

// hostEnrolmentSecretPrefix prefixes a Host's pins Secret name.
const hostEnrolmentSecretPrefix = "virtrigaud-agent-pins-"

// hostEnrolmentComponent is the app.kubernetes.io/component label of a pins
// Secret.
const hostEnrolmentComponent = "host-agent-pins"

// Event reasons of host-agent enrolment, emitted on the Host.
const (
	eventReasonAgentEnrolmentDenied   = "AgentEnrolmentDenied"
	eventReasonAgentEnrolmentApproved = "AgentEnrolmentApproved"
	eventReasonAgentEnrolled          = "AgentEnrolled"
	eventReasonAgentCertRejected      = "AgentCertificateRejected"
)

// Denial reasons of the Host checks (the request checks are hostidentity's
// Reason* constants). Each is set on the CertificateRequest's Denied
// condition.
const (
	// reasonEnrolHostUnnamed: the request does not name a Host.
	reasonEnrolHostUnnamed = "HostNotNamed"
	// reasonEnrolHostNotFound: the named Host does not exist or is deleting.
	reasonEnrolHostNotFound = "HostNotFound"
	// reasonEnrolHostNotAgent: the Host's Provider does not run a per-host
	// agent.
	reasonEnrolHostNotAgent = "HostNotAgentManaged"
	// reasonEnrolIdentityMissing: the Host has no spec.identity.
	reasonEnrolIdentityMissing = "HostIdentityMissing"
	// reasonEnrolHostRevoked: the Host is revoked.
	reasonEnrolHostRevoked = "HostRevoked"
	// reasonEnrolNoPending: the manager drew no enrolment record for the Host.
	reasonEnrolNoPending = "NoPendingEnrolment"
	// reasonEnrolInFlight: another request for the Host is authorised and
	// not finished.
	reasonEnrolInFlight = "EnrolmentInFlight"
	// reasonEnrolAlreadyEnrolled: the Host is already enrolled; re-enrolment
	// is not supported by this manager.
	reasonEnrolAlreadyEnrolled = "AlreadyEnrolled"
	// reasonEnrolAuthorised is the Approved condition reason.
	reasonEnrolAuthorised = "VirtRigaudHostEnrolmentAuthorised"
)

// errForeignPinsSecret is returned when a Secret with a Host's pins Secret
// name exists but is not a manager-owned pins Secret. The manager never
// overwrites it.
var errForeignPinsSecret = errors.New("a Secret with the host's pins Secret name exists and is not managed by VirtRigaud")

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
	// Namespace is where enrolment CertificateRequests live.
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
	// CABundle is the PEM bundle of the subordinate CA.
	CABundle []byte
	// InstallID is the 16-byte install identifier (from the development
	// operation signer).
	InstallID []byte
}

// ParseHostEnrolmentConfig turns the flags into a configuration. With no
// flag set it returns nil and no error (enrolment disabled). A release build
// refuses any flag. Otherwise the namespace, the admin group, the trust
// domain, the CA bundle, at least one issuer and the install id (a loaded
// development signer) are required.
func ParseHostEnrolmentConfig(f HostEnrolmentFlags, installID []byte) (*HostEnrolmentConfig, error) {
	if !f.isSet() {
		return nil, nil
	}
	if !devgate.Compiled {
		return nil, fmt.Errorf("the --%s* flags cannot be used with a release build: %w", "dev-host-enrolment-", devgate.ErrNotCompiled)
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
		ref, err := certrequest.ParseIssuerRef(f.ExternalPKIIssuer)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", DevHostEnrolmentExternalPKIIssuerFlagName, err)
		}
		cfg.ExternalPKIIssuer = &ref
	}
	if cfg.CertManagerIssuer == nil && cfg.ExternalPKIIssuer == nil {
		return nil, fmt.Errorf("--%s or --%s is required", DevHostEnrolmentIssuerFlagName, DevHostEnrolmentExternalPKIIssuerFlagName)
	}
	if cfg.CertManagerIssuer != nil && cfg.ExternalPKIIssuer != nil && *cfg.CertManagerIssuer == *cfg.ExternalPKIIssuer {
		return nil, errors.New("the cert-manager and external-PKI issuer references must differ")
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
// the manager has recorded an outcome.
func (externalPKIIssuer) Issued(cr *unstructured.Unstructured) ([]byte, error) {
	if _, _, done := certrequest.Condition(cr, certrequest.ConditionReady); done {
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
// in its pins Secret. It holds public identifiers and public keys' pins
// only.
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
	// RetiredEnrolmentIDs are enrolment ids already used for the host; none
	// is drawn again.
	RetiredEnrolmentIDs []string `json:"retiredEnrolmentIDs,omitempty"`
	// LastDenial is the last denied request, for the Host condition.
	LastDenial *requestOutcome `json:"lastDenial,omitempty"`
	// LastRejection is the last refused issued certificate, for the Host
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

// currentEnrolment is a pinned enrolment.
type currentEnrolment struct {
	Record      enrolmentRecord `json:"record"`
	KeyPins     []string        `json:"keyPins"`
	CABundle    []byte          `json:"caBundle"`
	ExpectedSAN string          `json:"expectedSAN"`
	NotAfter    metav1.Time     `json:"notAfter"`
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

// hostEnrolmentSecretName returns the name of a Host's pins Secret.
func hostEnrolmentSecretName(hostName string) string {
	return hostEnrolmentSecretPrefix + hostName
}

// loadedEnrolment is a Host's state and the Secret it was read from (nil
// when absent).
type loadedEnrolment struct {
	state  hostEnrolmentState
	secret *corev1.Secret
	// reset reports that a stored state for another Host object or
	// identity was discarded.
	reset bool
}

// loadHostEnrolment reads a Host's pins Secret. A missing Secret yields a
// fresh state. A Secret owned by another Host object, or whose state names
// another UID or identity (a replaced host), yields a fresh state with reset
// set: the old pins are never inherited. A Secret not managed by VirtRigaud
// is refused (errForeignPinsSecret).
func loadHostEnrolment(ctx context.Context, c client.Reader, host *infravirtrigaudiov1beta1.Host) (loadedEnrolment, error) {
	fresh := hostEnrolmentState{HostUID: string(host.UID), Identity: host.Spec.Identity}
	sec := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: hostEnrolmentSecretName(host.Name)}, sec)
	if apierrors.IsNotFound(err) {
		return loadedEnrolment{state: fresh}, nil
	}
	if err != nil {
		return loadedEnrolment{}, fmt.Errorf("get pins Secret of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	if sec.Labels[k8sLabelComponent] != hostEnrolmentComponent || sec.Labels[k8sLabelManagedBy] != k8sManagedByVirtRigaud {
		return loadedEnrolment{}, fmt.Errorf("Host %s/%s: %w", host.Namespace, host.Name, errForeignPinsSecret)
	}
	var st hostEnrolmentState
	if err := json.Unmarshal(sec.Data[hostEnrolmentStateKey], &st); err != nil ||
		!metav1.IsControlledBy(sec, host) || st.HostUID != string(host.UID) || st.Identity != host.Spec.Identity {
		return loadedEnrolment{state: fresh, secret: sec, reset: true}, nil
	}
	return loadedEnrolment{state: st, secret: sec}, nil
}

// Label keys and values of manager-owned objects.
const (
	k8sLabelComponent      = "app.kubernetes.io/component"
	k8sLabelManagedBy      = "app.kubernetes.io/managed-by"
	k8sManagedByVirtRigaud = "virtrigaud"
)

// saveHostEnrolment writes a Host's state to its pins Secret, creating it
// when absent. The Secret is controlled by the Host (garbage-collected with
// it), and an update carries the read resourceVersion, so a concurrent
// writer makes it fail with a conflict instead of being overwritten.
func saveHostEnrolment(ctx context.Context, c client.Client, scheme *runtime.Scheme, host *infravirtrigaudiov1beta1.Host, l *loadedEnrolment) error {
	data, err := json.Marshal(l.state)
	if err != nil {
		return fmt.Errorf("marshal enrolment state of Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	sec := l.secret
	create := sec == nil
	if create {
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: host.Namespace, Name: hostEnrolmentSecretName(host.Name)}}
	} else {
		sec = sec.DeepCopy()
	}
	if sec.Labels == nil {
		sec.Labels = map[string]string{}
	}
	sec.Labels[k8sLabelComponent] = hostEnrolmentComponent
	sec.Labels[k8sLabelManagedBy] = k8sManagedByVirtRigaud
	sec.Type = corev1.SecretTypeOpaque
	sec.Data = map[string][]byte{hostEnrolmentStateKey: data}
	if !metav1.IsControlledBy(sec, host) {
		// A Secret left by a replaced Host (same name, another UID) is taken
		// over with its state discarded (loadHostEnrolment reset it).
		sec.OwnerReferences = nil
		if err := controllerutil.SetControllerReference(host, sec, scheme); err != nil {
			return fmt.Errorf("own pins Secret of Host %s/%s: %w", host.Namespace, host.Name, err)
		}
	}
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

// HostEnrolmentReconciler draws a Host's enrolment record, keeps its pins
// Secret bound to the Host object and identity, and mirrors the Secret into
// Host.status.agent and the AgentEnrolled condition. Development builds
// only: the manager registers it only with a HostEnrolmentConfig.
type HostEnrolmentReconciler struct {
	client.Client
	// Scheme sets owner references.
	Scheme *runtime.Scheme
	// Config is the enrolment configuration.
	Config *HostEnrolmentConfig
	// Recorder emits events on Hosts (nil: none).
	Recorder record.EventRecorder
	// Rand draws enrolment ids (nil: crypto/rand).
	Rand io.Reader
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch

// Reconcile implements reconcile.Reconciler.
func (r *HostEnrolmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !host.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil || !agent {
		return ctrl.Result{}, err
	}
	if host.Spec.Identity == "" {
		return ctrl.Result{}, r.mirror(ctx, host, nil)
	}
	l, err := loadHostEnrolment(ctx, r.Client, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	changed := l.reset
	if l.reset {
		log.FromContext(ctx).Info("Discarded the enrolment state of a previous Host object or identity; no pin is inherited",
			"host", req.NamespacedName.String())
	}
	if !host.Spec.Revoked && l.state.Current == nil {
		if p := l.state.Pending; p == nil || p.ProviderUID != string(provider.UID) || p.InstallID != hex.EncodeToString(r.Config.InstallID) {
			id, err := r.drawEnrolmentID(l.state.RetiredEnrolmentIDs)
			if err != nil {
				return ctrl.Result{}, err
			}
			l.state.Pending = &enrolmentRecord{EnrolmentID: id, ProviderUID: string(provider.UID), InstallID: hex.EncodeToString(r.Config.InstallID)}
			l.state.Authorised = nil
			changed = true
		}
	}
	if changed {
		if err := saveHostEnrolment(ctx, r.Client, r.Scheme, host, &l); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, r.mirror(ctx, host, &l.state)
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
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentRevoked, "spec.revoked is true; no enrolment is authorised"
		case st.Current != nil:
			cond.Status = metav1.ConditionTrue
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentEnrolled, "the agent certificate is validated and its key is pinned"
		case st.Authorised != nil:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentAwaitingCertificate,
				fmt.Sprintf("certificate request %s is authorised; waiting for the issued certificate", st.Authorised.Request)
		case st.LastRejection != nil:
			cond.Reason, cond.Message = infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected,
				fmt.Sprintf("the certificate issued for request %s was refused: %s", st.LastRejection.Request, st.LastRejection.Message)
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

// SetupWithManager registers the reconciler: Hosts, and the pins Secrets they
// own (so a Secret change is mirrored at once).
func (r *HostEnrolmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.Host{}).
		Owns(&corev1.Secret{}).
		Named("host-enrolment").
		Complete(r)
}

// HostEnrolmentRequestReconciler decides enrolment CertificateRequests
// (approve or deny) and pins the key of a validated issued certificate.
// Development builds only: the manager registers it only with a
// HostEnrolmentConfig.
type HostEnrolmentRequestReconciler struct {
	client.Client
	// Scheme sets owner references.
	Scheme *runtime.Scheme
	// Config is the enrolment configuration.
	Config *HostEnrolmentConfig
	// Recorder emits events on Hosts (nil: none).
	Recorder record.EventRecorder
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements reconcile.Reconciler.
func (r *HostEnrolmentRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Config.Namespace {
		return ctrl.Result{}, nil
	}
	cr := certrequest.NewObject()
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !certrequest.IsEnrolment(cr) || !cr.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	var iss HostCertificateIssuer
	for _, i := range r.Config.issuers() {
		if i.Handles(cr) {
			iss = i
			break
		}
	}
	if iss == nil {
		return ctrl.Result{}, nil
	}
	switch {
	case !certrequest.Decided(cr):
		return ctrl.Result{}, r.decide(ctx, cr, iss)
	case certrequest.IsTrue(cr, certrequest.ConditionApproved) && !certrequest.Failed(cr):
		return ctrl.Result{}, r.complete(ctx, cr, iss)
	}
	return ctrl.Result{}, nil
}

// now returns the current time.
func (r *HostEnrolmentRequestReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// decide authorises an undecided request or denies it.
func (r *HostEnrolmentRequestReconciler) decide(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return r.deny(ctx, cr, nil, nil, reasonEnrolHostUnnamed, "the request does not name a Host in annotation "+certrequest.AnnotationHost)
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		if apierrors.IsNotFound(err) {
			return r.deny(ctx, cr, nil, nil, reasonEnrolHostNotFound, fmt.Sprintf("Host %s/%s does not exist", ns, name))
		}
		return fmt.Errorf("get Host of request %s: %w", reqName, err)
	}
	if !host.DeletionTimestamp.IsZero() {
		return r.deny(ctx, cr, nil, nil, reasonEnrolHostNotFound, fmt.Sprintf("Host %s/%s is being deleted", ns, name))
	}
	_, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil {
		return err
	}
	if !agent {
		return r.deny(ctx, cr, host, nil, reasonEnrolHostNotAgent, "the Host's Provider does not run a per-host agent")
	}
	if host.Spec.Identity == "" {
		return r.deny(ctx, cr, host, nil, reasonEnrolIdentityMissing, "the Host has no spec.identity")
	}
	l, err := loadHostEnrolment(ctx, r.Client, host)
	if err != nil {
		return err
	}
	if host.Spec.Revoked {
		if l.reset {
			return r.deny(ctx, cr, host, nil, reasonEnrolHostRevoked, "the Host is revoked")
		}
		return r.deny(ctx, cr, host, &l, reasonEnrolHostRevoked, "the Host is revoked")
	}
	if l.reset {
		// The Host reconciler discards the stale state; until then there is
		// no pending record for this Host object.
		return r.deny(ctx, cr, host, nil, reasonEnrolNoPending, "no enrolment record is drawn for this Host")
	}
	if l.state.Current != nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolAlreadyEnrolled, "the Host is already enrolled")
	}
	if l.state.Pending == nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolNoPending, "no enrolment record is drawn for this Host")
	}
	if a := l.state.Authorised; a != nil && a.RequestUID != string(cr.GetUID()) {
		live, err := r.authorisedRequestLive(ctx, a)
		if err != nil {
			return err
		}
		if live {
			return r.deny(ctx, cr, host, &l, reasonEnrolInFlight, "another request for this Host is authorised: "+a.Request)
		}
		l.state.Authorised = nil
	}
	rec, err := l.state.Pending.toRecord()
	if err != nil {
		return fmt.Errorf("pending record of Host %s/%s: %w", ns, name, err)
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolHostNotFound, err.Error())
	}
	in, err := certrequest.RequestOf(cr)
	if err != nil {
		return r.deny(ctx, cr, host, &l, hostidentity.ReasonRequestInvalid, err.Error())
	}
	auth, err := hostidentity.Authorise(hostidentity.Policy{
		AdminGroup: r.Config.AdminGroup, HostName: host.Name, HostURI: hostURI, Record: rec,
	}, in)
	if err != nil {
		if d, ok := hostidentity.AsDenial(err); ok {
			return r.deny(ctx, cr, host, &l, d.Reason, d.Message)
		}
		return err
	}
	// Record the authorisation first, then approve: a crash in between
	// leaves an authorised, undecided request, which the next reconcile
	// approves (its UID matches).
	l.state.Authorised = &authorisedRequest{
		Request: reqName, RequestUID: string(cr.GetUID()), Issuer: iss.Name(), Record: *l.state.Pending, KeyPin: auth.KeyPin,
	}
	l.state.LastDenial, l.state.LastRejection = nil, nil
	if err := saveHostEnrolment(ctx, r.Client, r.Scheme, host, &l); err != nil {
		return err
	}
	if err := certrequest.SetCondition(cr, certrequest.ConditionApproved, certrequest.StatusTrue, reasonEnrolAuthorised,
		"authorised by VirtRigaud for Host "+ns+"/"+name, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("approve request %s: %w", reqName, err)
	}
	log.FromContext(ctx).Info("Approved a host-agent enrolment request", "request", reqName, "host", ns+"/"+name, "issuer", iss.Name())
	r.event(host, corev1.EventTypeNormal, eventReasonAgentEnrolmentApproved, "certificate request "+reqName+" approved")
	return nil
}

// authorisedRequestLive reports whether the authorised request still exists
// and has not failed.
func (r *HostEnrolmentRequestReconciler) authorisedRequestLive(ctx context.Context, a *authorisedRequest) (bool, error) {
	other := certrequest.NewObject()
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: nameOf(a.Request)}, other)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get authorised request %s: %w", a.Request, err)
	}
	return string(other.GetUID()) == a.RequestUID && !certrequest.Failed(other), nil
}

// nameOf returns the name part of "namespace/name".
func nameOf(nsName string) string {
	for i := len(nsName) - 1; i >= 0; i-- {
		if nsName[i] == '/' {
			return nsName[i+1:]
		}
	}
	return nsName
}

// deny sets the request's Denied condition and, when the Host's state is
// loaded, records the denial in it (never touching the pins).
func (r *HostEnrolmentRequestReconciler) deny(ctx context.Context, cr *unstructured.Unstructured, host *infravirtrigaudiov1beta1.Host, l *loadedEnrolment, reason, message string) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	if l != nil && host != nil {
		l.state.LastDenial = &requestOutcome{Request: reqName, Reason: reason}
		if err := saveHostEnrolment(ctx, r.Client, r.Scheme, host, l); err != nil {
			return err
		}
	}
	if err := certrequest.SetCondition(cr, certrequest.ConditionDenied, certrequest.StatusTrue, reason, message, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("deny request %s: %w", reqName, err)
	}
	log.FromContext(ctx).Info("Denied a host-agent enrolment request", "request", reqName, "reason", reason)
	if host != nil {
		r.event(host, corev1.EventTypeWarning, eventReasonAgentEnrolmentDenied, "certificate request "+reqName+" denied: "+reason)
	}
	return nil
}

// complete validates the issued certificate of the authorised request and
// pins its key, or refuses it.
func (r *HostEnrolmentRequestReconciler) complete(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return nil
	}
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err != nil {
		return client.IgnoreNotFound(err)
	}
	l, err := loadHostEnrolment(ctx, r.Client, host)
	if err != nil {
		return err
	}
	a := l.state.Authorised
	if l.reset || a == nil || a.RequestUID != string(cr.GetUID()) || a.Issuer != iss.Name() {
		return nil
	}
	chain, err := iss.Issued(cr)
	if err != nil || chain == nil {
		return err
	}
	issued, verr := r.validateIssued(ctx, host, a, chain)
	if verr != nil {
		l.state.Authorised = nil
		l.state.LastRejection = &requestOutcome{Request: reqName, Reason: infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected, Message: verr.Error()}
	} else {
		if l.state.Current != nil {
			l.state.RetiredEnrolmentIDs = appendUnique(l.state.RetiredEnrolmentIDs, l.state.Current.Record.EnrolmentID)
		}
		l.state.Current = &currentEnrolment{
			Record:      a.Record,
			KeyPins:     []string{a.KeyPin},
			CABundle:    append([]byte(nil), r.Config.CABundle...),
			ExpectedSAN: issued.HostURI,
			NotAfter:    metav1.NewTime(issued.Leaf.NotAfter.UTC().Truncate(time.Second)),
		}
		l.state.RetiredEnrolmentIDs = appendUnique(l.state.RetiredEnrolmentIDs, a.Record.EnrolmentID)
		l.state.Pending, l.state.Authorised, l.state.LastDenial, l.state.LastRejection = nil, nil, nil, nil
	}
	if err := saveHostEnrolment(ctx, r.Client, r.Scheme, host, &l); err != nil {
		return err
	}
	changed, err := iss.Complete(cr, verr, r.now())
	if err != nil {
		return err
	}
	if changed {
		if err := r.Status().Update(ctx, cr); err != nil {
			return fmt.Errorf("record the outcome on request %s: %w", reqName, err)
		}
	}
	if verr != nil {
		log.FromContext(ctx).Info("Refused the certificate issued for a host-agent enrolment request", "request", reqName, "host", ns+"/"+name)
		r.event(host, corev1.EventTypeWarning, eventReasonAgentCertRejected, "the certificate issued for request "+reqName+" was refused")
		return nil
	}
	log.FromContext(ctx).Info("Pinned a host-agent key", "request", reqName, "host", ns+"/"+name, "enrolmentID", a.Record.EnrolmentID)
	r.event(host, corev1.EventTypeNormal, eventReasonAgentEnrolled, "agent key pinned from request "+reqName)
	return nil
}

// validateIssued re-checks the Host (still an approved, unrevoked agent
// host) and validates the chain against the authorisation and the
// subordinate CA.
func (r *HostEnrolmentRequestReconciler) validateIssued(ctx context.Context, host *infravirtrigaudiov1beta1.Host, a *authorisedRequest, chain []byte) (issuedAgentCert, error) {
	if host.Spec.Revoked || !host.DeletionTimestamp.IsZero() || host.Spec.Identity == "" {
		return issuedAgentCert{}, errors.New("the Host is revoked, deleting or has no identity")
	}
	if _, agent, err := hostRunsAgent(ctx, r.Client, host); err != nil || !agent {
		return issuedAgentCert{}, errors.New("the Host's Provider does not run a per-host agent")
	}
	rec, err := a.Record.toRecord()
	if err != nil {
		return issuedAgentCert{}, err
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return issuedAgentCert{}, err
	}
	roots, err := hostidentity.RootPool(r.Config.CABundle)
	if err != nil {
		return issuedAgentCert{}, err
	}
	iss, err := hostidentity.ValidateIssued(hostidentity.Authorisation{HostURI: hostURI, Record: rec, KeyPin: a.KeyPin}, chain, roots, r.now())
	if err != nil {
		return issuedAgentCert{}, err
	}
	return issuedAgentCert{Issued: iss, HostURI: hostURI}, nil
}

// issuedAgentCert is a validated issued certificate and the host URI it was
// validated for.
type issuedAgentCert struct {
	hostidentity.Issued
	HostURI string
}

// appendUnique appends s unless present.
func appendUnique(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// event emits an event on the Host when a recorder is set.
func (r *HostEnrolmentRequestReconciler) event(host *infravirtrigaudiov1beta1.Host, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(host, typ, reason, msg)
	}
}

// SetupWithManager registers the reconciler on CertificateRequests. The
// manager's cache must restrict CertificateRequests to the enrolment
// namespace (HostEnrolmentCacheOptions).
func (r *HostEnrolmentRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(certrequest.NewObject()).
		Named("host-enrolment-request").
		Complete(r)
}

// resolveHostAgentTLS returns the agentTLS credentials of a Host whose
// Provider runs a per-host agent, read from its pins Secret, or a non-secret
// skip reason. Release builds render no agent credentials.
func resolveHostAgentTLS(ctx context.Context, c client.Reader, host *infravirtrigaudiov1beta1.Host) (hostsecret.Credentials, string) {
	switch {
	case !devgate.Compiled:
		return hostsecret.Credentials{}, "per-host agent hosts are rendered only by development builds"
	case host.Spec.Revoked:
		return hostsecret.Credentials{}, "host is revoked"
	case host.Spec.Identity == "":
		return hostsecret.Credentials{}, "host has no spec.identity"
	}
	l, err := loadHostEnrolment(ctx, c, host)
	if err != nil {
		if errors.Is(err, errForeignPinsSecret) {
			return hostsecret.Credentials{}, "host pins Secret is not managed by VirtRigaud"
		}
		return hostsecret.Credentials{}, "host pins Secret unreadable"
	}
	if l.reset || l.state.Current == nil {
		return hostsecret.Credentials{}, "host agent is not enrolled"
	}
	cur := l.state.Current
	creds := hostsecret.Credentials{AgentTLS: &hostsecret.AgentTLS{
		CABundle: cur.CABundle, ExpectedSAN: cur.ExpectedSAN, KeyPins: append([]string(nil), cur.KeyPins...),
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
