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

package hostidentity

import (
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// StoreAttestedLedgerVersion is the first HostLedgerState content version
// that carries the store's attestation record (fields 11 to 14). A state of
// a lower version leaves them unset: the agent that signed it does not
// report the record, so its absence says nothing.
const StoreAttestedLedgerVersion uint32 = 2

// storeIDLen is the length of an enrolment identifier in the record.
const storeIDLen = 16

// StoreAttestation is the store's attestation record a verified
// HostLedgerState reports. It is evidence only: nothing in this package
// decides on it.
type StoreAttestation struct {
	// Reported is true when the state's version carries the record
	// (StoreAttestedLedgerVersion or higher). When false every other field
	// is zero and says nothing about the store.
	Reported bool
	// EverAttested is true when the store has ever been attested against a
	// host ledger.
	EverAttested bool
	// FirstSeq is the ledger position sequence number of the store's first
	// attestation; 0 when EverAttested is false.
	FirstSeq uint64
	// FirstEnrolmentID is the enrolment active at the first attestation
	// (16 bytes); nil when EverAttested is false or none was active.
	FirstEnrolmentID []byte
	// RecordSeq is the chain sequence number of the store transaction that
	// committed the record; 0 when EverAttested is false.
	RecordSeq uint64
}

// StoreAttestationOf decodes the store's attestation record from l, the
// HostLedgerState of an answer whose signature the caller verified. It
// refuses (ErrAnswer, "no answer") a state that breaks the record's field
// rules: any of fields 11 to 14 set at a version below
// StoreAttestedLedgerVersion; store_ever_attested false with any other
// field set; store_ever_attested true with store_first_attested_seq 0, a
// store_attested_record_seq not above it, or an enrolment that is neither
// empty nor 16 bytes. A nil state is refused.
func StoreAttestationOf(l *providerv1.HostLedgerState) (StoreAttestation, error) {
	if l == nil {
		return StoreAttestation{}, refuse("no host ledger state")
	}
	anySet := l.GetStoreEverAttested() || l.GetStoreFirstAttestedSeq() != 0 ||
		len(l.GetStoreFirstAttestedEnrolmentId()) != 0 || l.GetStoreAttestedRecordSeq() != 0
	if l.GetVersion() < StoreAttestedLedgerVersion {
		if anySet {
			return StoreAttestation{}, refuse("a host ledger state of version %d sets the attestation record (11-14)", l.GetVersion())
		}
		return StoreAttestation{}, nil
	}
	if !l.GetStoreEverAttested() {
		if anySet {
			return StoreAttestation{}, refuse("a store never attested reports attestation facts")
		}
		return StoreAttestation{Reported: true}, nil
	}
	first, rec, enrol := l.GetStoreFirstAttestedSeq(), l.GetStoreAttestedRecordSeq(), l.GetStoreFirstAttestedEnrolmentId()
	switch {
	case first == 0:
		return StoreAttestation{}, refuse("an attested store reports no first attestation position")
	case rec <= first:
		return StoreAttestation{}, refuse("the attestation record's sequence number %d is not above the first attestation's %d", rec, first)
	case len(enrol) != 0 && len(enrol) != storeIDLen:
		return StoreAttestation{}, refuse("the first attestation's enrolment is %d bytes", len(enrol))
	}
	a := StoreAttestation{Reported: true, EverAttested: true, FirstSeq: first, RecordSeq: rec}
	if len(enrol) != 0 {
		a.FirstEnrolmentID = append([]byte(nil), enrol...)
	}
	return a, nil
}
