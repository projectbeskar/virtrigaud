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

// Package operation mints and verifies operation grants (ADR-0016 Part A,
// docs/operation-grants-wire-shape.md): the signed GrantBody that binds one
// mutating Provider call to its exact request bytes.
//
// It holds three things:
//
//   - the OperationKind table (grantverify/kinds.go): each kind names exactly one RPC, its
//     request message type and, for the four Power kinds, the fixed
//     PowerRequest.op;
//   - grant minting (grant.go): MintOperationGrant builds a version 1
//     GRANT_KIND_OPERATION body, serialises it once and has a Signer sign
//     those exact bytes;
//   - one verifier (grantverify/verify.go, re-exported by reexport.go) that a
//     test fake and a later per-host agent share. It fails closed: an unknown grant_version, an unknown field in
//     the envelope, the grant, the body, any message nested in it or the
//     decoded request, an UNSPECIFIED or unknown enum value, a zero counter,
//     a placeholder field set at version 1, a kind that does not fit the RPC
//     or the request, and an outer request field set beside the envelope are
//     all refused, with the reason GRANT_UNAUTHENTICATED, GRANT_MISMATCH or
//     IDENTITY_REQUIRED.
//
// Signing is development-only. The only Signer implementation, DevSigner, is
// compiled in only under the virtrigaud_dev build tag (signer_dev.go). A
// release build has no signer: SignerCompiled is false, LoadSigner refuses
// any key file with ErrNoSigner, and the manager's transport refuses to send
// a signed operation. No built-in Provider reports operation identity, and no
// controller uses this package yet.
//
// Request bytes can carry secrets (for example cloud-init user data), and a
// signer holds key material; neither is ever logged or put in an error.
//
// Run the tagged tests with: go test -tags virtrigaud_dev ./internal/operation/
package operation
