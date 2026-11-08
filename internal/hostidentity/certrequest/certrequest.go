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

// Package certrequest reads and writes cert-manager CertificateRequest
// objects as unstructured objects, for per-host agent enrolment (development
// builds only). It deliberately does not import the cert-manager API module:
// the enrolment path touches a handful of fields (spec.request,
// spec.issuerRef, spec.duration, spec.usages, spec.isCA, spec.username,
// spec.groups, status.conditions, status.certificate, status.ca), and an
// unstructured object reads them without pulling cert-manager's dependency
// tree into the manager and the CLI.
package certrequest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

const (
	// Group is the cert-manager API group.
	Group = "cert-manager.io"
	// Version is the cert-manager API version.
	Version = "v1"
	// Kind is the CertificateRequest kind.
	Kind = "CertificateRequest"
	// ListKind is the CertificateRequestList kind.
	ListKind = "CertificateRequestList"

	// LabelAgentEnrolment marks a CertificateRequest as a per-host agent
	// enrolment request (value "true"); the manager lists requests by it.
	LabelAgentEnrolment = "enrolment.virtrigaud.io/agent"
	// AnnotationHost names the Host a request enrols, as "namespace/name". An
	// annotation, not a label: a Host name can be longer than a label value.
	AnnotationHost = "enrolment.virtrigaud.io/host"

	// ConditionApproved is cert-manager's approval condition type.
	ConditionApproved = "Approved"
	// ConditionDenied is cert-manager's denial condition type.
	ConditionDenied = "Denied"
	// ConditionReady is cert-manager's issuance condition type.
	ConditionReady = "Ready"
	// ConditionInvalidRequest is cert-manager's invalid-request condition type.
	ConditionInvalidRequest = "InvalidRequest"

	// StatusTrue and StatusFalse are condition statuses.
	StatusTrue  = "True"
	StatusFalse = "False"

	// ReasonIssued is the Ready=True reason cert-manager uses.
	ReasonIssued = "Issued"
	// ReasonFailed is the Ready=False reason for a terminal failure.
	ReasonFailed = "Failed"

	// labelTrue is the value of LabelAgentEnrolment.
	labelTrue = "true"
)

// GVK is the CertificateRequest GroupVersionKind.
var GVK = schema.GroupVersionKind{Group: Group, Version: Version, Kind: Kind}

// ListGVK is the CertificateRequestList GroupVersionKind.
var ListGVK = schema.GroupVersionKind{Group: Group, Version: Version, Kind: ListKind}

// IssuerRef is a CertificateRequest's spec.issuerRef.
type IssuerRef struct {
	// Group is the issuer's API group (cert-manager.io for cert-manager's own
	// issuers).
	Group string
	// Kind is the issuer's kind (Issuer, ClusterIssuer, ...).
	Kind string
	// Name is the issuer's name.
	Name string
}

// String renders the reference as group/kind/name.
func (r IssuerRef) String() string { return r.Group + "/" + r.Kind + "/" + r.Name }

// ParseIssuerRef parses "kind/name" (group cert-manager.io) or
// "group/kind/name".
func ParseIssuerRef(s string) (IssuerRef, error) {
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return IssuerRef{Group: Group, Kind: parts[0], Name: parts[1]}, nil
	case len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "":
		return IssuerRef{Group: parts[0], Kind: parts[1], Name: parts[2]}, nil
	}
	return IssuerRef{}, fmt.Errorf("issuer reference %q is not kind/name or group/kind/name", s)
}

// NewObject returns an empty CertificateRequest object.
func NewObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(GVK)
	return u
}

// NewList returns an empty CertificateRequestList object.
func NewList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(ListGVK)
	return l
}

// Spec is what an enrolment CertificateRequest asks for.
type Spec struct {
	// Namespace is the issuer namespace the request is created in.
	Namespace string
	// GenerateName prefixes the generated request name.
	GenerateName string
	// HostNamespace and HostName name the Host being enrolled.
	HostNamespace, HostName string
	// IssuerRef is the issuer.
	IssuerRef IssuerRef
	// CSRPEM is the PEM certificate signing request.
	CSRPEM []byte
	// Duration is the requested lifetime.
	Duration time.Duration
}

// New builds an enrolment CertificateRequest: the agent label, the Host
// annotation, the issuer, the CSR, the lifetime, and the client and server
// authentication usages. The requester fields are left to cert-manager's
// admission webhook, which records the authenticated creator.
func New(s Spec) *unstructured.Unstructured {
	u := NewObject()
	u.SetNamespace(s.Namespace)
	u.SetGenerateName(s.GenerateName)
	u.SetLabels(map[string]string{LabelAgentEnrolment: labelTrue})
	u.SetAnnotations(map[string]string{AnnotationHost: s.HostNamespace + "/" + s.HostName})
	u.Object["spec"] = map[string]any{
		"request":  base64.StdEncoding.EncodeToString(s.CSRPEM),
		"duration": s.Duration.String(),
		"isCA":     false,
		"usages":   []any{hostidentity.UsageDigitalSignature, hostidentity.UsageClientAuth, hostidentity.UsageServerAuth},
		"issuerRef": map[string]any{
			"group": s.IssuerRef.Group,
			"kind":  s.IssuerRef.Kind,
			"name":  s.IssuerRef.Name,
		},
	}
	return u
}

// IsEnrolment reports whether the object carries the agent enrolment label.
func IsEnrolment(u *unstructured.Unstructured) bool {
	return u.GetLabels()[LabelAgentEnrolment] == labelTrue
}

// HostOf returns the Host a request names in its annotation.
func HostOf(u *unstructured.Unstructured) (namespace, name string, ok bool) {
	ns, n, found := strings.Cut(u.GetAnnotations()[AnnotationHost], "/")
	if !found || ns == "" || n == "" || strings.Contains(n, "/") {
		return "", "", false
	}
	return ns, n, true
}

// IssuerRefOf returns the request's spec.issuerRef. An empty group means
// cert-manager.io, as cert-manager reads it.
func IssuerRefOf(u *unstructured.Unstructured) IssuerRef {
	g, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "group")
	k, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "kind")
	n, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "name")
	if g == "" {
		g = Group
	}
	if k == "" {
		k = "Issuer"
	}
	return IssuerRef{Group: g, Kind: k, Name: n}
}

// RequestOf reads the fields the authorisation policy checks.
func RequestOf(u *unstructured.Unstructured) (hostidentity.Request, error) {
	var req hostidentity.Request
	enc, _, err := unstructured.NestedString(u.Object, "spec", "request")
	if err != nil {
		return req, errors.New("spec.request is not a string")
	}
	csr, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return req, errors.New("spec.request is not base64")
	}
	req.CSRPEM = csr
	if d, found, err := unstructured.NestedString(u.Object, "spec", "duration"); err != nil {
		return req, errors.New("spec.duration is not a string")
	} else if found && d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			return req, errors.New("spec.duration is not a duration")
		}
		req.Duration = dur
	}
	if req.Usages, _, err = unstructured.NestedStringSlice(u.Object, "spec", "usages"); err != nil {
		return req, errors.New("spec.usages is not a list of strings")
	}
	if req.IsCA, _, err = unstructured.NestedBool(u.Object, "spec", "isCA"); err != nil {
		return req, errors.New("spec.isCA is not a boolean")
	}
	if req.Username, _, err = unstructured.NestedString(u.Object, "spec", "username"); err != nil {
		return req, errors.New("spec.username is not a string")
	}
	if req.Groups, _, err = unstructured.NestedStringSlice(u.Object, "spec", "groups"); err != nil {
		return req, errors.New("spec.groups is not a list of strings")
	}
	if req.UID, _, err = unstructured.NestedString(u.Object, "spec", "uid"); err != nil {
		return req, errors.New("spec.uid is not a string")
	}
	extra, found, err := unstructured.NestedMap(u.Object, "spec", "extra")
	if err != nil {
		return req, errors.New("spec.extra is not a map")
	}
	if found {
		req.Extra = make(map[string][]string, len(extra))
		for k := range extra {
			vals, _, err := unstructured.NestedStringSlice(extra, k)
			if err != nil {
				return req, errors.New("spec.extra values are not lists of strings")
			}
			req.Extra[k] = vals
		}
	}
	return req, nil
}

// Condition returns the status and reason of a condition, and whether it is
// present.
func Condition(u *unstructured.Unstructured, condType string) (status, reason string, found bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		s, _ := m["status"].(string)
		r, _ := m["reason"].(string)
		return s, r, true
	}
	return "", "", false
}

// IsTrue reports whether a condition is present with status True.
func IsTrue(u *unstructured.Unstructured, condType string) bool {
	s, _, found := Condition(u, condType)
	return found && s == StatusTrue
}

// Decided reports whether the request is approved or denied.
func Decided(u *unstructured.Unstructured) bool {
	return IsTrue(u, ConditionApproved) || IsTrue(u, ConditionDenied)
}

// Failed reports whether the request reached a terminal failure (denied,
// invalid, or Ready=False with reason Failed).
func Failed(u *unstructured.Unstructured) bool {
	if IsTrue(u, ConditionDenied) || IsTrue(u, ConditionInvalidRequest) {
		return true
	}
	s, r, found := Condition(u, ConditionReady)
	return found && s == StatusFalse && r == ReasonFailed
}

// SetCondition sets (adding or replacing) a condition in status.conditions.
// The transition time changes only when the status does.
func SetCondition(u *unstructured.Unstructured, condType, status, reason, message string, now time.Time) error {
	conds, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("status.conditions: %w", err)
	}
	ts := now.UTC().Format(time.RFC3339)
	entry := map[string]any{"type": condType, "status": status, "reason": reason, "message": message, "lastTransitionTime": ts}
	replaced := false
	for i, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		if m["status"] == status {
			if old, ok := m["lastTransitionTime"].(string); ok {
				entry["lastTransitionTime"] = old
			}
		}
		conds[i] = entry
		replaced = true
	}
	if !replaced {
		conds = append(conds, entry)
	}
	return unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
}

// Certificate returns status.certificate (PEM), or nil when unset.
func Certificate(u *unstructured.Unstructured) ([]byte, error) {
	enc, found, err := unstructured.NestedString(u.Object, "status", "certificate")
	if err != nil {
		return nil, errors.New("status.certificate is not a string")
	}
	if !found || enc == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, errors.New("status.certificate is not base64")
	}
	return b, nil
}

// SetCertificate sets status.certificate (and status.ca when caPEM is not
// empty).
func SetCertificate(u *unstructured.Unstructured, certPEM, caPEM []byte) error {
	if err := unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString(certPEM), "status", "certificate"); err != nil {
		return fmt.Errorf("set status.certificate: %w", err)
	}
	if len(caPEM) > 0 {
		if err := unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString(caPEM), "status", "ca"); err != nil {
			return fmt.Errorf("set status.ca: %w", err)
		}
	}
	return nil
}

// CSR returns spec.request (PEM).
func CSR(u *unstructured.Unstructured) ([]byte, error) {
	enc, _, err := unstructured.NestedString(u.Object, "spec", "request")
	if err != nil {
		return nil, errors.New("spec.request is not a string")
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, errors.New("spec.request is not base64")
	}
	return b, nil
}

// Bundle is what an operator submits to an external PKI for an authorised
// request: the CSR and the issuance policy the returned certificate must
// meet. It carries no private key.
type Bundle struct {
	// Host is the Host being enrolled, "namespace/name".
	Host string `json:"host"`
	// Request is the authorised CertificateRequest, "namespace/name".
	Request string `json:"request"`
	// CSR is the PEM certificate signing request.
	CSR string `json:"csr"`
	// SubjectAltNameURIs are the exact URI subject alternative names the
	// certificate must carry, and no other subject alternative name.
	SubjectAltNameURIs []string `json:"subjectAltNameURIs"`
	// ExtendedKeyUsages are the usages the certificate must carry.
	ExtendedKeyUsages []string `json:"extendedKeyUsages"`
	// MaxLifetime is the longest lifetime the certificate may have.
	MaxLifetime string `json:"maxLifetime"`
	// CA is false: the certificate must not be a CA.
	CA bool `json:"ca"`
}
