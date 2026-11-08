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

// Package grantverify is the stateless operation-grant verifier and the
// OperationKind table it is built on. It is the one decoder of an operation
// envelope: the manager's test fakes and the per-host agent both use it, and
// a caller acts only on Verified.Request.
//
// The package is deliberately small in what it links. It imports the generated
// provider protocol messages and the Go standard library and nothing else of
// this repository: not the Kubernetes API types, not controller-runtime, not
// the provider contract package. The refusal reasons are therefore plain
// strings (ReasonGrantUnauthenticated, ReasonGrantMismatch,
// ReasonIdentityRequired), pinned to the contract package's constants by a
// test. An import-boundary test fails the build if that changes.
//
// Verification reads the time from a Clock. WallClock is the default; a
// per-host agent passes a clock that never moves behind a persisted floor
// (see internal/agent/clockfloor), so a rolled-back wall clock cannot revive
// an expired signing key.
//
// internal/operation re-exports this package's identifiers, so existing
// callers keep their import path.
package grantverify
