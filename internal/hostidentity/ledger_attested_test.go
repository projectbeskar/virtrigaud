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

package hostidentity_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func attestedState() *providerv1.HostLedgerState {
	return &providerv1.HostLedgerState{RequestedVersion: 2, Version: 2,
		State: providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION, StoreEverAttested: true,
		StoreFirstAttestedSeq: 9, StoreFirstAttestedEnrolmentId: bytes.Repeat([]byte{4}, 16), StoreAttestedRecordSeq: 10}
}

func TestStoreAttestationOf(t *testing.T) {
	got, err := hostidentity.StoreAttestationOf(attestedState())
	if err != nil || !got.Reported || !got.EverAttested || got.FirstSeq != 9 || got.RecordSeq != 10 ||
		!bytes.Equal(got.FirstEnrolmentID, bytes.Repeat([]byte{4}, 16)) {
		t.Fatalf("attested: %+v %v", got, err)
	}

	noEnrol := attestedState()
	noEnrol.StoreFirstAttestedEnrolmentId = nil
	if got, err := hostidentity.StoreAttestationOf(noEnrol); err != nil || !got.EverAttested || got.FirstEnrolmentID != nil {
		t.Fatalf("attested without an enrolment: %+v %v", got, err)
	}

	never := &providerv1.HostLedgerState{RequestedVersion: 2, Version: 2}
	if got, err := hostidentity.StoreAttestationOf(never); err != nil || !got.Reported || got.EverAttested {
		t.Fatalf("never attested: %+v %v", got, err)
	}

	// Version 1 does not report the record: false there says nothing.
	v1 := &providerv1.HostLedgerState{RequestedVersion: 1, Version: 1}
	if got, err := hostidentity.StoreAttestationOf(v1); err != nil || got.Reported || got.EverAttested {
		t.Fatalf("version 1: %+v %v", got, err)
	}

	for name, edit := range map[string]func(*providerv1.HostLedgerState){
		"record at version 1":    func(l *providerv1.HostLedgerState) { l.Version = 1 },
		"seq at version 1":       func(l *providerv1.HostLedgerState) { l.Version, l.StoreEverAttested = 1, false },
		"facts without the flag": func(l *providerv1.HostLedgerState) { l.StoreEverAttested = false },
		"only an enrolment": func(l *providerv1.HostLedgerState) {
			l.StoreEverAttested, l.StoreFirstAttestedSeq, l.StoreAttestedRecordSeq = false, 0, 0
		},
		"flag without a position":    func(l *providerv1.HostLedgerState) { l.StoreFirstAttestedSeq = 0 },
		"record at the position":     func(l *providerv1.HostLedgerState) { l.StoreAttestedRecordSeq = 9 },
		"record below the position":  func(l *providerv1.HostLedgerState) { l.StoreAttestedRecordSeq = 3 },
		"short enrolment":            func(l *providerv1.HostLedgerState) { l.StoreFirstAttestedEnrolmentId = []byte{1} },
		"long enrolment":             func(l *providerv1.HostLedgerState) { l.StoreFirstAttestedEnrolmentId = bytes.Repeat([]byte{1}, 17) },
		"record seq without a flag":  func(l *providerv1.HostLedgerState) { l.StoreEverAttested, l.StoreFirstAttestedSeq = false, 0 },
		"flag at version 0 (absent)": func(l *providerv1.HostLedgerState) { l.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			l := attestedState()
			edit(l)
			if _, err := hostidentity.StoreAttestationOf(l); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("StoreAttestationOf = %v, want ErrAnswer", err)
			}
		})
	}
	if _, err := hostidentity.StoreAttestationOf(nil); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("nil state: %v", err)
	}
}
