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

// Forward compatibility of the per-Host state the manager persists
// (development builds only): the enrolment state and store positions in a
// Host's pins Secret.
//
// A newer manager may add members to those documents, data keys to the pins
// Secret, and Secrets of its own for a Host. Two rules keep an older manager
// (one an operator rolled back to) from destroying that state:
//
//   - Preservation: a read-modify-write keeps every JSON member it does not
//     know, at every nesting level, and every pins Secret data key it does
//     not own. Only a state discarded because it belongs to another Host
//     object or identity drops them, as it drops everything else.
//   - Downgrade floor: a document may carry a top-level member
//     minManagerVersion (a semantic version). A manager whose own version is
//     below it, whose own version is not a semantic version (a development
//     build), or that cannot parse the floor, refuses the Host: it neither
//     writes nor deletes the pins Secret, approves nothing for the Host and
//     verifies no answer of it, and reports the refusal on the Host. The same
//     holds for a floor in any Secret of the enrolment namespace labelled
//     LabelHostLedger whose Host annotation names the Host, or that names no
//     Host (it then applies to every Host).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"

	"github.com/blang/semver/v4"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/version"
)

// LabelHostLedger is the label key of a Secret in the enrolment namespace
// that holds additional per-Host state written by a newer manager. This
// manager creates none; it only honours a downgrade floor in one (see
// minManagerVersionMember).
const LabelHostLedger = "virtrigaud.io/host-ledger"

// minManagerVersionMember is the top-level JSON member of a persisted
// per-Host document that names the lowest manager version allowed to act on
// it.
const minManagerVersionMember = "minManagerVersion"

// reasonEnrolManagerTooOld is the AgentEnrolled condition reason, and the
// event reason, of a Host whose persisted state requires a newer manager.
const reasonEnrolManagerTooOld = "ManagerTooOld"

// errManagerTooOld is returned when a Host's persisted state requires a newer
// manager than this one, or carries a floor this manager cannot read. The
// manager then leaves the Host's state untouched.
var errManagerTooOld = errors.New("the host's persisted enrolment state requires a newer manager")

// buildVersion returns this manager's version (set at build time); a
// variable so tests can pin it.
var buildVersion = func() string { return version.Version }

// parseManagerVersion parses a semantic version with an optional leading
// "v".
func parseManagerVersion(s string) (semver.Version, error) {
	return semver.Parse(strings.TrimPrefix(strings.TrimSpace(s), "v"))
}

// checkManagerFloor checks the downgrade floor of one persisted document,
// named what (for the error). A document that is not a JSON object, or has
// no floor member, has no floor. A floor that is not a semantic version
// string, a floor above this manager's version, or any floor when this
// manager's version is not a semantic version, is refused with an error
// wrapping errManagerTooOld. Versions compare by semantic version precedence,
// so a pre-release or a build described from a tag (v1.2.3-4-gabcdef) is
// below the release it names.
func checkManagerFloor(what string, doc []byte) error {
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return nil //nolint:nilerr // not an object: no floor (the caller decodes it on its own terms)
	}
	raw, ok := top[minManagerVersionMember]
	if !ok {
		return nil
	}
	var floorText string
	if err := json.Unmarshal(raw, &floorText); err != nil {
		return fmt.Errorf("%s: %s is not a string: %w", what, minManagerVersionMember, errManagerTooOld)
	}
	floor, err := parseManagerVersion(floorText)
	if err != nil {
		return fmt.Errorf("%s: %s %q is not a semantic version: %w", what, minManagerVersionMember, floorText, errManagerTooOld)
	}
	own, err := parseManagerVersion(buildVersion())
	if err != nil {
		return fmt.Errorf("%s requires manager %s or later and this manager's version %q is not a semantic version: %w",
			what, floor, buildVersion(), errManagerTooOld)
	}
	if own.LT(floor) {
		return fmt.Errorf("%s requires manager %s or later; this manager is %s: %w", what, floor, own, errManagerTooOld)
	}
	return nil
}

// checkPinsSecretFloor checks the floors of the documents in a pins Secret.
func checkPinsSecretFloor(sec *corev1.Secret) error {
	if sec == nil {
		return nil
	}
	for _, key := range []string{hostEnrolmentStateKey, hostStoreStateKey} {
		if err := checkManagerFloor("pins Secret key "+key, sec.Data[key]); err != nil {
			return err
		}
	}
	return nil
}

// checkHostLedgerFloors checks the floors in the host ledger Secrets of the
// enrolment namespace that apply to the Host namespace/name: every data value
// of a ledger Secret whose Host annotation names the Host or is absent. A
// listing failure is an error (the Host is not acted on).
func checkHostLedgerFloors(ctx context.Context, c client.Reader, cfg *HostEnrolmentConfig, namespace, name string) error {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(cfg.Namespace), client.HasLabels{LabelHostLedger}); err != nil {
		return fmt.Errorf("list host ledger Secrets for Host %s/%s: %w", namespace, name, err)
	}
	key := hostKey(namespace, name)
	for i := range list.Items {
		sec := &list.Items[i]
		if h, ok := sec.Annotations[certrequest.AnnotationHost]; ok && h != key {
			continue
		}
		for dataKey, v := range sec.Data {
			if err := checkManagerFloor(fmt.Sprintf("host ledger Secret %s key %s", sec.Name, dataKey), v); err != nil {
				return err
			}
		}
	}
	return nil
}

// unknownMembers are the members of a JSON object that the record type
// decoding it does not know, kept verbatim so that encoding the record
// writes them back.
type unknownMembers map[string]json.RawMessage

// knownMembersCache caches knownMembers per type.
var knownMembersCache sync.Map

// knownMembers returns the JSON member names of a struct type's fields.
func knownMembers(t reflect.Type) []string {
	if v, ok := knownMembersCache.Load(t); ok {
		if names, ok := v.([]string); ok {
			return names
		}
	}
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	knownMembersCache.Store(t, names)
	return names
}

// decodeKeepingUnknown decodes the JSON object data into v (a pointer to a
// struct type without its own UnmarshalJSON) and returns the members its
// fields do not take. Member names match fields case-insensitively, as
// encoding/json does, so a member a field takes is never also kept.
func decodeKeepingUnknown(data []byte, v any) (unknownMembers, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &all); err != nil {
		return nil, err
	}
	known := knownMembers(reflect.TypeOf(v).Elem())
	var out unknownMembers
	for k, raw := range all {
		if containsFold(known, k) {
			continue
		}
		if out == nil {
			out = unknownMembers{}
		}
		out[k] = raw
	}
	return out, nil
}

// encodeWithUnknown encodes v (a struct value of a type without its own
// MarshalJSON) and adds the unknown members it does not encode itself. With
// unknown members the object's members are in sorted order, so the output is
// deterministic either way.
func encodeWithUnknown(v any, unknown unknownMembers) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(unknown) == 0 {
		return b, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for k, raw := range unknown {
		if _, ok := all[k]; !ok {
			all[k] = raw
		}
	}
	return json.Marshal(all)
}

// equal reports whether two sets of unknown members are byte-for-byte equal.
func (u unknownMembers) equal(o unknownMembers) bool {
	return maps.EqualFunc(u, o, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) })
}

// containsFold reports whether names holds s, ignoring case.
func containsFold(names []string, s string) bool {
	for _, n := range names {
		if strings.EqualFold(n, s) {
			return true
		}
	}
	return false
}

// The record types of the persisted per-Host documents decode and encode
// through decodeKeepingUnknown and encodeWithUnknown, so a read-modify-write
// keeps the members a newer manager added at every level. Each method
// converts to a local type without the methods, so the standard encoding
// applies to the fields while nested records still use their own methods.

// UnmarshalJSON implements json.Unmarshaler.
func (st *hostEnrolmentState) UnmarshalJSON(b []byte) (err error) {
	type plain hostEnrolmentState
	st.Unknown, err = decodeKeepingUnknown(b, (*plain)(st))
	return err
}

// MarshalJSON implements json.Marshaler.
func (st hostEnrolmentState) MarshalJSON() ([]byte, error) {
	type plain hostEnrolmentState
	return encodeWithUnknown(plain(st), st.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *rekeyRequest) UnmarshalJSON(b []byte) (err error) {
	type plain rekeyRequest
	r.Unknown, err = decodeKeepingUnknown(b, (*plain)(r))
	return err
}

// MarshalJSON implements json.Marshaler.
func (r rekeyRequest) MarshalJSON() ([]byte, error) {
	type plain rekeyRequest
	return encodeWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *enrolmentRecord) UnmarshalJSON(b []byte) (err error) {
	type plain enrolmentRecord
	e.Unknown, err = decodeKeepingUnknown(b, (*plain)(e))
	return err
}

// MarshalJSON implements json.Marshaler.
func (e enrolmentRecord) MarshalJSON() ([]byte, error) {
	type plain enrolmentRecord
	return encodeWithUnknown(plain(e), e.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (a *authorisedRequest) UnmarshalJSON(b []byte) (err error) {
	type plain authorisedRequest
	a.Unknown, err = decodeKeepingUnknown(b, (*plain)(a))
	return err
}

// MarshalJSON implements json.Marshaler.
func (a authorisedRequest) MarshalJSON() ([]byte, error) {
	type plain authorisedRequest
	return encodeWithUnknown(plain(a), a.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *currentEnrolment) UnmarshalJSON(b []byte) (err error) {
	type plain currentEnrolment
	c.Unknown, err = decodeKeepingUnknown(b, (*plain)(c))
	return err
}

// MarshalJSON implements json.Marshaler.
func (c currentEnrolment) MarshalJSON() ([]byte, error) {
	type plain currentEnrolment
	return encodeWithUnknown(plain(c), c.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *requestOutcome) UnmarshalJSON(b []byte) (err error) {
	type plain requestOutcome
	r.Unknown, err = decodeKeepingUnknown(b, (*plain)(r))
	return err
}

// MarshalJSON implements json.Marshaler.
func (r requestOutcome) MarshalJSON() ([]byte, error) {
	type plain requestOutcome
	return encodeWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *renewalState) UnmarshalJSON(b []byte) (err error) {
	type plain renewalState
	r.Unknown, err = decodeKeepingUnknown(b, (*plain)(r))
	return err
}

// MarshalJSON implements json.Marshaler.
func (r renewalState) MarshalJSON() ([]byte, error) {
	type plain renewalState
	return encodeWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *renewalAttempt) UnmarshalJSON(b []byte) (err error) {
	type plain renewalAttempt
	r.Unknown, err = decodeKeepingUnknown(b, (*plain)(r))
	return err
}

// MarshalJSON implements json.Marshaler.
func (r renewalAttempt) MarshalJSON() ([]byte, error) {
	type plain renewalAttempt
	return encodeWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *storeRecordJSON) UnmarshalJSON(b []byte) (err error) {
	type plain storeRecordJSON
	s.Unknown, err = decodeKeepingUnknown(b, (*plain)(s))
	return err
}

// MarshalJSON implements json.Marshaler.
func (s storeRecordJSON) MarshalJSON() ([]byte, error) {
	type plain storeRecordJSON
	return encodeWithUnknown(plain(s), s.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (h *hostStoreStateJSON) UnmarshalJSON(b []byte) (err error) {
	type plain hostStoreStateJSON
	h.Unknown, err = decodeKeepingUnknown(b, (*plain)(h))
	return err
}

// MarshalJSON implements json.Marshaler.
func (h hostStoreStateJSON) MarshalJSON() ([]byte, error) {
	type plain hostStoreStateJSON
	return encodeWithUnknown(plain(h), h.Unknown)
}
