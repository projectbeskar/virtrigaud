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

package contracts

import (
	"context"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// LedgerPosition is a position of a host's operation store as the host
// ledger records it.
type LedgerPosition struct {
	// StoreID is the 16-byte store identifier.
	StoreID []byte
	// Seq is the chain sequence number, at least 1.
	Seq uint64
	// Head is the 32-byte chain head at Seq.
	Head []byte
}

// ChallengeQuery asks a host's agent for an attestation challenge.
type ChallengeQuery struct {
	// HostID is the host.
	HostID string
	// Purpose is ATTEST (the agent awaits its first attestation) or
	// REATTEST (it is attested in a session the manager does not hold).
	Purpose providerv1.ChallengePurpose
}

// AttestOrder is what an attestation grant names. The caller builds it
// only from the host ledger record its own successful write returned.
type AttestOrder struct {
	// HostID, ProviderUID and EnrolmentID name the host and its active
	// enrolment.
	HostID      string
	ProviderUID string
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch OperationEpoch
	// ChallengeNonce is the nonce of the host's signed challenge.
	ChallengeNonce []byte
	// Ledger is the ledger position the host attests against.
	Ledger LedgerPosition
	// Checkpoint is the ledger checkpoint (nil: none).
	Checkpoint *LedgerPosition
}

// LedgerMarkOrder is what a ledger mark grant names. The caller builds it
// only from the host ledger record its own successful write returned.
type LedgerMarkOrder struct {
	// HostID, ProviderUID and EnrolmentID name the host and its active
	// enrolment.
	HostID      string
	ProviderUID string
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch OperationEpoch
	// AttestationID is the session the mark is for.
	AttestationID []byte
	// Ledger is the position the host's mark is raised to.
	Ledger LedgerPosition
}

// AuthorityRead is the result of one host authority call: the signed read
// (nonce and relayed answer) and, for a call that carried a grant, the
// grant's digest the answer must name.
type AuthorityRead struct {
	SignedRead
	// GrantDigest is the digest of the grant sent (nil for a challenge).
	GrantDigest []byte
}

// HostAuthorityCaller is an optional capability of a Provider client: the
// host authority service of the host ledger protocol (development builds).
// Each call draws a fresh nonce; an attestation or ledger mark grant is
// minted by the client's signer over exactly what the order names. A
// release build, or a client with no signer, refuses and sends nothing.
type HostAuthorityCaller interface {
	// Challenge asks the host for an attestation challenge.
	Challenge(ctx context.Context, q ChallengeQuery) (AuthorityRead, error)
	// Attest delivers an attestation grant.
	Attest(ctx context.Context, o AttestOrder) (AuthorityRead, error)
	// LedgerMark delivers a ledger mark grant.
	LedgerMark(ctx context.Context, o LedgerMarkOrder) (AuthorityRead, error)
}
