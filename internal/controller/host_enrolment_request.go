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
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// Denial reasons of the requester and Host checks (the request checks are
// hostidentity's Reason* constants). Each is set on the CertificateRequest's
// Denied condition.
const (
	// reasonEnrolHostUnnamed: the request does not name a Host.
	reasonEnrolHostUnnamed = "HostNotNamed"
	// reasonEnrolNotAllowedForHost: the requester is not allowed the enrol
	// verb on the named Host (SubjectAccessReview).
	reasonEnrolNotAllowedForHost = "RequesterNotAuthorisedForHost"
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
	// reasonEnrolRecordRetired: the record the request was built for was
	// retired after an abandoned issuance; a new record is drawn.
	reasonEnrolRecordRetired = "EnrolmentRecordRetired"
	// reasonEnrolInFlight: another request for the Host is authorised and
	// not finished.
	reasonEnrolInFlight = "EnrolmentInFlight"
	// reasonEnrolAlreadyEnrolled: the Host is already enrolled; re-enrolment
	// is not supported by this manager.
	reasonEnrolAlreadyEnrolled = "AlreadyEnrolled"
	// reasonEnrolAuthorised is the Approved condition reason.
	reasonEnrolAuthorised = "VirtRigaudHostEnrolmentAuthorised"
)

// HostEnrolmentRequestReconciler decides enrolment CertificateRequests
// (approve or deny) and pins the key of a validated issued certificate.
// Development builds only: the manager registers it only with a
// HostEnrolmentConfig.
type HostEnrolmentRequestReconciler struct {
	client.Client
	// Config is the enrolment configuration.
	Config *HostEnrolmentConfig
	// Recorder emits events on Hosts (nil: none).
	Recorder record.EventRecorder
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
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

// requesterAllowed asks the API server whether the request's recorded
// requester may use the enrol verb on the Host namespace/name.
func (r *HostEnrolmentRequestReconciler) requesterAllowed(ctx context.Context, in hostidentity.Request, namespace, name string) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(in.Extra))
	for k, v := range in.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: in.Username, UID: in.UID, Groups: in.Groups, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: infravirtrigaudiov1beta1.HostEnrolVerb,
			Group: infravirtrigaudiov1beta1.GroupVersion.Group, Resource: "hosts", Name: name,
		},
	}}
	if err := r.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("review the requester's %s permission on Host %s/%s: %w", infravirtrigaudiov1beta1.HostEnrolVerb, namespace, name, err)
	}
	return sar.Status.Allowed && !sar.Status.Denied, nil
}

// decide authorises an undecided request or denies it. The requester is
// checked first (group, then the enrol verb on the Host), before any Host,
// Provider or Secret is read: a request from anyone else is denied with no
// Host lookup, no state write and no Host event.
func (r *HostEnrolmentRequestReconciler) decide(ctx context.Context, cr *unstructured.Unstructured, iss HostCertificateIssuer) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	ns, name, ok := certrequest.HostOf(cr)
	if !ok {
		return r.deny(ctx, cr, nil, nil, reasonEnrolHostUnnamed, "the request does not name a Host in annotation "+certrequest.AnnotationHost)
	}
	in, err := certrequest.RequestOf(cr)
	if err != nil {
		return r.deny(ctx, cr, nil, nil, hostidentity.ReasonRequestInvalid, err.Error())
	}
	if !slices.Contains(in.Groups, r.Config.AdminGroup) {
		return r.deny(ctx, cr, nil, nil, hostidentity.ReasonRequesterNotAuthorised, "the requester is not in the enrolment administrator group")
	}
	allowed, err := r.requesterAllowed(ctx, in, ns, name)
	if err != nil {
		return err
	}
	if !allowed {
		return r.deny(ctx, cr, nil, nil, reasonEnrolNotAllowedForHost,
			fmt.Sprintf("the requester may not %s Host %s/%s", infravirtrigaudiov1beta1.HostEnrolVerb, ns, name))
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
	if host.Spec.Revoked {
		return r.deny(ctx, cr, host, nil, reasonEnrolHostRevoked, "the Host is revoked")
	}
	l, err := loadHostEnrolment(ctx, r.Client, r.Config, host)
	if err != nil {
		return err
	}
	if l.reset {
		// The Host reconciler discards the stale state; until then there is
		// no pending record for this Host object.
		return r.deny(ctx, cr, host, nil, reasonEnrolNoPending, "no enrolment record is drawn for this Host")
	}
	if l.state.Current != nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolAlreadyEnrolled, "the Host is already enrolled")
	}
	if a := l.state.Authorised; a != nil && a.RequestUID != string(cr.GetUID()) {
		live, err := enrolmentRequestLive(ctx, r.Client, r.Config, a)
		if err != nil {
			return err
		}
		if live {
			return r.deny(ctx, cr, host, &l, reasonEnrolInFlight, "another request for this Host is authorised: "+a.Request)
		}
		// The authorised request was abandoned: its record is retired, never
		// handed to another key; the Host reconciler draws a new one.
		l.state.LastRejection = &requestOutcome{Request: a.Request, Reason: infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected,
			Message: "the authorised request was deleted or failed before a certificate was pinned"}
		l.state.retireOpenRecords()
		return r.deny(ctx, cr, host, &l, reasonEnrolRecordRetired,
			"the enrolment record was retired after an abandoned request; build a new request from status.agent.requestURIs")
	}
	if l.state.Pending == nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolNoPending, "no enrolment record is drawn for this Host")
	}
	rec, err := l.state.Pending.toRecord()
	if err != nil {
		return fmt.Errorf("pending record of Host %s/%s: %w", ns, name, err)
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return r.deny(ctx, cr, host, &l, reasonEnrolHostNotFound, err.Error())
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
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
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

// deny sets the request's Denied condition. With a Host, it emits a Warning
// event on it; with its loaded state, it also records the denial (never
// touching the pins). Both are passed only once the requester is
// authorised for the Host.
func (r *HostEnrolmentRequestReconciler) deny(ctx context.Context, cr *unstructured.Unstructured, host *infravirtrigaudiov1beta1.Host, l *loadedEnrolment, reason, message string) error {
	reqName := cr.GetNamespace() + "/" + cr.GetName()
	if l != nil && host != nil {
		l.state.LastDenial = &requestOutcome{Request: reqName, Reason: reason}
		if err := saveHostEnrolment(ctx, r.Client, r.Config, host, l); err != nil {
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
// pins its key, or refuses it (retiring its record).
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
	l, err := loadHostEnrolment(ctx, r.Client, r.Config, host)
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
		l.state.LastRejection = &requestOutcome{Request: reqName, Reason: infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected, Message: verr.Error()}
		// The refused certificate is CA-valid and carries this record, so the
		// record is retired and never pinned to another key.
		l.state.retireOpenRecords()
	} else {
		l.state.Current = &currentEnrolment{
			Record:   a.Record,
			KeyPins:  []string{a.KeyPin},
			NotAfter: metav1.NewTime(issued.Leaf.NotAfter.UTC().Truncate(time.Second)),
		}
		l.state.retire(a.Record.EnrolmentID)
		l.state.Pending, l.state.Authorised, l.state.LastDenial, l.state.LastRejection = nil, nil, nil, nil
	}
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
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
// configured subordinate CA.
func (r *HostEnrolmentRequestReconciler) validateIssued(ctx context.Context, host *infravirtrigaudiov1beta1.Host, a *authorisedRequest, chain []byte) (hostidentity.Issued, error) {
	if host.Spec.Revoked || !host.DeletionTimestamp.IsZero() || host.Spec.Identity == "" {
		return hostidentity.Issued{}, errors.New("the Host is revoked, deleting or has no identity")
	}
	if _, agent, err := hostRunsAgent(ctx, r.Client, host); err != nil || !agent {
		return hostidentity.Issued{}, errors.New("the Host's Provider does not run a per-host agent")
	}
	rec, err := a.Record.toRecord()
	if err != nil {
		return hostidentity.Issued{}, err
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return hostidentity.Issued{}, err
	}
	roots, err := hostidentity.RootPool(r.Config.CABundle)
	if err != nil {
		return hostidentity.Issued{}, err
	}
	return hostidentity.ValidateIssued(hostidentity.Authorisation{HostName: host.Name, HostURI: hostURI, Record: rec, KeyPin: a.KeyPin},
		chain, roots, r.now())
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
