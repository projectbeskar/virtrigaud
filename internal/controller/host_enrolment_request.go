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
	"encoding/hex"
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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	// reasonEnrolProviderChanged: the Host is enrolled with another Provider
	// than its own. A re-key never changes the Provider; also the
	// AgentEnrolled condition reason of such a Host.
	reasonEnrolProviderChanged = "ProviderChanged"
	// reasonEnrolTooManyPredecessors: the Host already holds the most
	// predecessor enrolments whose freeze is not confirmed.
	reasonEnrolTooManyPredecessors = "TooManyPredecessorEnrolments"
	// reasonEnrolEpochExhausted: no later start epoch is left for the Host.
	reasonEnrolEpochExhausted = "StartEpochExhausted"
	// reasonEnrolNotAllowedToRekey: the requester is not allowed the rekey
	// verb on the enrolled Host.
	reasonEnrolNotAllowedToRekey = "RequesterNotAuthorisedToRekey"
	// reasonEnrolNoRekeyWindow: the Host is enrolled and no re-key window is
	// open (HostRekeyRequestAnnotation).
	reasonEnrolNoRekeyWindow = "NoRekeyWindow"
	// reasonEnrolKeyPreviouslyPinned: the request's key is, or was, pinned
	// for the Host; a re-key needs a new key.
	reasonEnrolKeyPreviouslyPinned = "KeyPreviouslyPinned"
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
	// Admission gates approvals on the periodic admission check (nil: no
	// gate; tests only — the manager always sets it).
	Admission *AdmissionGate
	// APIReader reads the pins Secret of a renewal decision from the API
	// server, so a renewal is never denied or withdrawn on a stale cache
	// (nil: the Client; tests only).
	APIReader client.Reader
}

// liveReader returns the APIReader, or the Client when none is set.
func (r *HostEnrolmentRequestReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// admissionSuspendedRequeue is how soon an undecided request is retried
// while approvals are suspended.
const admissionSuspendedRequeue = time.Minute

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements reconcile.Reconciler. An optimistic-lock conflict
// (the request approved by another approver, or the pins Secret written,
// since it was read) is requeued at once without an error log: the next
// reconcile re-reads the request and the pins Secret and decides again. A
// request another approver approved is not re-decided (it is decided); the
// manager's own record of its approval decides whether the issued
// certificate is ever published (completeRenewal), so a request it would
// have denied is never accepted.
func (r *HostEnrolmentRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return requeueOnConflict(r.reconcile(ctx, req))
}

// reconcile is Reconcile without the conflict handling.
func (r *HostEnrolmentRequestReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Config.Namespace {
		return ctrl.Result{}, nil
	}
	cr := certrequest.NewObject()
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	renewal := certrequest.IsRenewal(cr)
	if (!certrequest.IsEnrolment(cr) && !renewal) || !cr.GetDeletionTimestamp().IsZero() {
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
	if renewal {
		// A request that names itself a renewal is judged only as one: it is
		// approved only when the manager created it (decideRenewal).
		switch {
		case !certrequest.Decided(cr):
			if r.Admission != nil && !r.Admission.OK() {
				log.FromContext(ctx).Info("Not deciding a host-agent renewal request while approvals are suspended", "request", req.String())
				return ctrl.Result{RequeueAfter: admissionSuspendedRequeue}, nil
			}
			return ctrl.Result{}, r.decideRenewal(ctx, cr, iss)
		case certrequest.IsTrue(cr, certrequest.ConditionApproved) && !certrequest.Failed(cr):
			// A request the manager refused (or that was denied) stays
			// refused: nothing is judged again, whatever changed since.
			if certrequest.IsTrue(cr, certrequest.ConditionRefused) || certrequest.IsTrue(cr, certrequest.ConditionDenied) {
				return ctrl.Result{}, nil
			}
			// Approved by another approver before the manager decided it:
			// apply the manager's rules first (re-read, no condition but a
			// refusal written), under the same admission gate as a request
			// it decides itself. One already issued before that is not
			// judged: completeRenewal refuses it (no approval of the
			// manager's is recorded).
			host, unjudged, err := r.approvedElsewhereUnjudged(ctx, cr)
			if err != nil {
				return ctrl.Result{}, err
			}
			if unjudged && renewalRequestFinished(cr) {
				// Issued before the manager judged it: completeRenewal
				// refuses it; the Host says so, as for a refusal.
				r.event(host, corev1.EventTypeWarning, eventReasonAgentRenewalDenied, "certificate renewal request "+req.String()+
					" was approved by another approver and issued before the manager judged it: its certificate is not published")
			}
			if unjudged && !renewalRequestFinished(cr) {
				if r.Admission != nil && !r.Admission.OK() {
					log.FromContext(ctx).Info("Not judging a host-agent renewal request another approver approved while approvals are suspended",
						"request", req.String())
					return ctrl.Result{RequeueAfter: admissionSuspendedRequeue}, nil
				}
				if err := r.judgeRenewal(ctx, cr, iss, true); err != nil {
					return ctrl.Result{}, err
				}
				if certrequest.IsTrue(cr, certrequest.ConditionRefused) {
					return ctrl.Result{}, nil
				}
			}
			return ctrl.Result{}, r.completeRenewal(ctx, cr, iss)
		}
		return ctrl.Result{}, nil
	}
	switch {
	case !certrequest.Decided(cr):
		if r.Admission != nil && !r.Admission.OK() {
			// The requester fields cannot be trusted without the admission
			// webhooks: decide nothing until the check passes again.
			log.FromContext(ctx).Info("Not deciding a host-agent enrolment request while approvals are suspended", "request", req.String())
			return ctrl.Result{RequeueAfter: admissionSuspendedRequeue}, nil
		}
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
// requester may use verb (enrol or rekey) on the Host namespace/name.
func (r *HostEnrolmentRequestReconciler) requesterAllowed(ctx context.Context, in hostidentity.Request, verb, namespace, name string) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(in.Extra))
	for k, v := range in.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: in.Username, UID: in.UID, Groups: in.Groups, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb,
			Group: infravirtrigaudiov1beta1.GroupVersion.Group, Resource: "hosts", Name: name,
		},
	}}
	if err := r.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("review the requester's %s permission on Host %s/%s: %w", verb, namespace, name, err)
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
	// The requester must hold enrol or rekey on the Host before the Host is
	// read; which one the request needs is decided once the Host's state is
	// known (rekey for an enrolled Host).
	mayEnrol, err := r.requesterAllowed(ctx, in, infravirtrigaudiov1beta1.HostEnrolVerb, ns, name)
	if err != nil {
		return err
	}
	mayRekey := false
	if !mayEnrol {
		if mayRekey, err = r.requesterAllowed(ctx, in, infravirtrigaudiov1beta1.HostRekeyVerb, ns, name); err != nil {
			return err
		}
	}
	if !mayEnrol && !mayRekey {
		return r.deny(ctx, cr, nil, nil, reasonEnrolNotAllowedForHost,
			fmt.Sprintf("the requester may neither %s nor %s Host %s/%s", infravirtrigaudiov1beta1.HostEnrolVerb,
				infravirtrigaudiov1beta1.HostRekeyVerb, ns, name))
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
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
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
	if l.state.Current == nil {
		// A Host that holds no enrolment is not enrolled while another live
		// Host claims its identity (the first holder wins); the denial says
		// no more than that. The decision lists every Host live.
		dups, err := hostsWithIdentity(ctx, r.liveReader(), nil, host)
		if err != nil {
			return err
		}
		if len(dups) > 0 {
			return r.deny(ctx, cr, host, nil, reasonEnrolRefused, "no enrolment can be authorised for this Host now")
		}
	}
	if l.reset {
		// The Host reconciler discards the stale state; until then there is
		// no pending record for this Host object.
		return r.deny(ctx, cr, host, nil, reasonEnrolNoPending, "no enrolment record is drawn for this Host")
	}
	// An enrolled Host may be re-keyed (a new key or a reinstalled or
	// restored host: a new enrolment of the same identity) under exactly the
	// rules of a first enrolment, except that a re-key never changes the
	// Provider and is refused while the predecessor list is full.
	if l.state.Current != nil {
		if reason, msg := l.state.rekeyBlocked(string(provider.UID)); reason != "" {
			return r.deny(ctx, cr, host, &l, reason, msg)
		}
		if mayEnrol {
			if mayRekey, err = r.requesterAllowed(ctx, in, infravirtrigaudiov1beta1.HostRekeyVerb, ns, name); err != nil {
				return err
			}
		}
		if !mayRekey {
			return r.deny(ctx, cr, host, &l, reasonEnrolNotAllowedToRekey,
				fmt.Sprintf("the Host is enrolled: a re-key needs the %s verb on Host %s/%s", infravirtrigaudiov1beta1.HostRekeyVerb, ns, name))
		}
		if !l.state.rekeyOpen(r.now()) {
			return r.deny(ctx, cr, host, &l, reasonEnrolNoRekeyWindow,
				"the Host is enrolled and no re-key window is open: set annotation "+infravirtrigaudiov1beta1.HostRekeyRequestAnnotation+
					" on the Host to the current time first")
		}
	} else if !mayEnrol {
		return r.deny(ctx, cr, host, &l, reasonEnrolNotAllowedForHost,
			fmt.Sprintf("the requester may not %s Host %s/%s", infravirtrigaudiov1beta1.HostEnrolVerb, ns, name))
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
	if l.state.keyPreviouslyPinned(auth.KeyPin) {
		return r.deny(ctx, cr, host, &l, reasonEnrolKeyPreviouslyPinned,
			"the request's key is, or was, pinned for this Host; a re-key needs a new key")
	}
	// Record the authorisation first, then approve: a crash in between
	// leaves an authorised, undecided request, which the next reconcile
	// approves (its UID matches). The record's start epoch counts as issued
	// from now on: a CA-valid certificate may exist for it even if the
	// manager later refuses it, so the next record starts after it.
	l.state.Authorised = &authorisedRequest{
		Request: reqName, RequestUID: string(cr.GetUID()), Issuer: iss.Name(), Record: *l.state.Pending, KeyPin: auth.KeyPin,
		Requester: in.Username,
	}
	l.state.LastIssuedEpoch = max(l.state.LastIssuedEpoch, l.state.Pending.epoch())
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
	log.FromContext(ctx).Info("Approved a host-agent enrolment request", "request", reqName, "host", ns+"/"+name, "issuer", iss.Name(),
		"rekey", l.state.Current != nil)
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
		return r.markWithdrawn(ctx, cr, host, &l)
	}
	chain, err := iss.Issued(cr)
	if err != nil || chain == nil {
		return err
	}
	issued, verr := r.validateIssued(ctx, host, a, chain)
	if errors.Is(verr, hostidentity.ErrLifetimeTooShort) {
		verr = fmt.Errorf("%s: %w", hostidentity.ReasonIssuedLifetimeTooShort, verr)
	}
	if verr == nil {
		if reason, msg := l.state.rekeyBlocked(a.Record.ProviderUID); reason != "" {
			verr = fmt.Errorf("%s: %s", reason, msg)
		} else if l.state.Current != nil && !l.state.rekeyOpen(r.now()) {
			verr = fmt.Errorf("%s: the re-key window closed before the certificate was issued", reasonEnrolNoRekeyWindow)
		}
	}
	var replaced string
	if verr != nil {
		l.state.LastRejection = &requestOutcome{Request: reqName, Reason: infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected, Message: verr.Error()}
		// The refused certificate is CA-valid and carries this record, so the
		// record is retired and never pinned to another key.
		l.state.retireOpenRecords()
	} else {
		// A re-key moves the replaced enrolment to the predecessors: its
		// attempts are still asked about and their VMs held, never re-sent.
		replaced = l.state.rekey(&currentEnrolment{
			Record:    a.Record,
			KeyPins:   []string{a.KeyPin},
			NotAfter:  metav1.NewTime(issued.Leaf.NotAfter.UTC().Truncate(time.Second)),
			NotBefore: ptrTime(issued.Leaf.NotBefore),
		}, r.now())
		l.state.retire(a.Record.EnrolmentID)
		l.state.Pending, l.state.Authorised, l.state.LastDenial, l.state.LastRejection = nil, nil, nil, nil
	}
	if err := saveHostEnrolment(ctx, r.Client, r.Config, host, &l); err != nil {
		return err
	}
	// The refusal is reported as soon as the state is saved, before the
	// request's status is written: a retry of a failed write finds the record
	// retired and marks the request withdrawn, so the event is only here.
	if verr != nil {
		log.FromContext(ctx).Info("Refused the certificate issued for a host-agent enrolment request", "request", reqName, "host", ns+"/"+name)
		if errors.Is(verr, hostidentity.ErrLifetimeTooShort) {
			r.event(host, corev1.EventTypeWarning, eventReasonAgentCertLifetimeTooShort, "the certificate issued for request "+reqName+
				" was refused and not pinned: "+verr.Error())
		} else {
			r.event(host, corev1.EventTypeWarning, eventReasonAgentCertRejected, "the certificate issued for request "+reqName+" was refused")
		}
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
		return nil
	}
	if replaced != "" {
		// Audit: identifiers and the public key pin only, never key or
		// certificate material.
		log.FromContext(ctx).Info("Re-keyed a host agent: the new key is pinned and the replaced enrolment is a predecessor",
			"request", reqName, "requester", a.Requester, "host", ns+"/"+name, "enrolmentID", a.Record.EnrolmentID,
			"replacedEnrolmentID", replaced, "keyPin", a.KeyPin, "startEpoch", l.state.Current.mintEpoch(),
			"predecessors", len(l.state.PredecessorEnrolmentIDs))
		r.event(host, corev1.EventTypeNormal, eventReasonAgentRekeyed, fmt.Sprintf("agent re-keyed by %s from request %s: enrolment %s "+
			"(start epoch %d) replaces %s, which is kept as a predecessor (its operations are asked about, never re-sent)",
			a.Requester, reqName, a.Record.EnrolmentID, l.state.Current.mintEpoch(), replaced))
		return nil
	}
	log.FromContext(ctx).Info("Pinned a host-agent key", "request", reqName, "host", ns+"/"+name, "enrolmentID", a.Record.EnrolmentID)
	r.event(host, corev1.EventTypeNormal, eventReasonAgentEnrolled, "agent key pinned from request "+reqName)
	return nil
}

// markWithdrawn marks as refused (certrequest.ConditionRefused) a request
// that holds an issued certificate the manager will never pin: its
// authorisation was withdrawn (a re-key window that closed, a retired
// record) and the certificate is not the Host's current one. A request
// already marked, or whose certificate is pinned, is left as it is.
func (r *HostEnrolmentRequestReconciler) markWithdrawn(ctx context.Context, cr *unstructured.Unstructured,
	host *infravirtrigaudiov1beta1.Host, l *loadedEnrolment) error {
	if certrequest.IsTrue(cr, certrequest.ConditionRefused) {
		return nil
	}
	chain, err := certrequest.Certificate(cr)
	if err != nil || len(chain) == 0 {
		return nil //nolint:nilerr // no issued certificate: nothing to mark
	}
	certs, err := hostidentity.ParseCertificates(chain)
	if err != nil {
		return nil //nolint:nilerr // not a certificate the manager could pin either
	}
	hostURI, err := hostidentity.HostURI(r.Config.TrustDomain, host.Namespace, host.Name)
	if err != nil {
		return nil //nolint:nilerr // a Host without a URI identity has no certificate to mark
	}
	rec, err := hostidentity.RecordFromURIs(certs[0].URIs, hostURI)
	if err != nil {
		return nil //nolint:nilerr // not a certificate for this Host
	}
	if c := l.state.Current; !l.reset && c != nil && c.Record.EnrolmentID == hex.EncodeToString(rec.EnrolmentID) {
		return nil
	}
	if !l.reset && l.state.Authorised != nil && l.state.Authorised.Record.EnrolmentID == hex.EncodeToString(rec.EnrolmentID) {
		return nil
	}
	why := errors.New("its authorisation was withdrawn before the certificate was pinned")
	if slices.Contains(l.state.PredecessorEnrolmentIDs, hex.EncodeToString(rec.EnrolmentID)) {
		why = errors.New("its enrolment was replaced by a re-key")
	}
	if err := markRefused(cr, why, r.now()); err != nil {
		return err
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("mark request %s/%s refused: %w", cr.GetNamespace(), cr.GetName(), err)
	}
	log.FromContext(ctx).Info("Marked a host-agent enrolment request whose certificate will never be pinned",
		"request", cr.GetNamespace()+"/"+cr.GetName(), "host", host.Namespace+"/"+host.Name)
	return nil
}

// validateIssued re-checks the Host (still an approved, unrevoked agent
// host) and validates the chain against the authorisation and the
// configured subordinate CA.
func (r *HostEnrolmentRequestReconciler) validateIssued(ctx context.Context, host *infravirtrigaudiov1beta1.Host, a *authorisedRequest, chain []byte) (hostidentity.Issued, error) {
	if host.Spec.Revoked || !host.DeletionTimestamp.IsZero() || host.Spec.Identity == "" {
		return hostidentity.Issued{}, errors.New("the Host is revoked, deleting or has no identity")
	}
	provider, agent, err := hostRunsAgent(ctx, r.Client, host)
	if err != nil || !agent {
		return hostidentity.Issued{}, errors.New("the Host's Provider does not run a per-host agent")
	}
	if a.Record.ProviderUID != string(provider.UID) {
		return hostidentity.Issued{}, errors.New("the authorised record names another Provider than the Host's")
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
	if r.Recorder != nil && host != nil {
		r.Recorder.Event(host, typ, reason, msg)
	}
}

// SetupWithManager registers the reconciler on CertificateRequests. The
// manager's cache must restrict CertificateRequests to the enrolment
// namespace (HostEnrolmentCacheOptions).
func (r *HostEnrolmentRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inEnrolmentNS := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == r.Config.Namespace })
	isPins := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[k8sLabelComponent] == hostEnrolmentComponent
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(certrequest.NewObject()).
		// A change of a pins Secret (a renewal withdrawn by a revoke, a
		// re-key or a new request) is seen by the requests it owns at once,
		// not at the next resync: completeRenewal marks a withdrawn
		// renewal's issued request refused. A heartbeat write of the store
		// positions alone does not enqueue (pinsTrustChanged).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsOwnedBy),
			builder.WithPredicates(inEnrolmentNS, isPins, pinsTrustChanged())).
		Named("host-enrolment-request").
		Complete(r)
}

// requestsOwnedBy maps a pins Secret to the CertificateRequests it owns (the
// manager's renewal requests) in the enrolment namespace.
func (r *HostEnrolmentRequestReconciler) requestsOwnedBy(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Config.Namespace || obj.GetUID() == "" {
		return nil
	}
	list := certrequest.NewList()
	if err := r.List(ctx, list, client.InNamespace(r.Config.Namespace), client.HasLabels{certrequest.LabelAgentRenewal}); err != nil {
		log.FromContext(ctx).V(1).Info("Could not list the renewal requests of a changed pins Secret; the next resync revisits them",
			"secret", obj.GetName(), "error", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		for _, o := range list.Items[i].GetOwnerReferences() {
			if o.UID == obj.GetUID() {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].GetNamespace(),
					Name: list.Items[i].GetName()}})
				break
			}
		}
	}
	return out
}
