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

// Package enrol is the agent half of host enrolment: it builds the host's
// certificate signing request and installs the certificate the manager's
// subordinate CA issued for it, then the daemon derives the host's
// enrolment record from that certificate.
//
// # Who does what
//
// Enrolment spans two users, because the host private key belongs to the
// signer process's user and the agent user must never read it:
//
//   - vrtg-agent enroll csr runs as the signer's user. It creates the host
//     key (signer.CreateKey: ECDSA P-256, mode 0600, in the signer's key
//     directory) and signs the certificate signing request with it in the
//     same process (BuildCSR). The request is public and is written to a
//     file for an administrator.
//   - vrtg-agent enroll install runs as the agent's user. It reads the
//     issued certificate and the request file, asks the running signer for
//     its public key over the socket (never the key file), and installs the
//     certificate (Install) only when every check passes.
//
// The request is signed where the key is created, not through the signer's
// socket: the socket is a digest oracle with budgets for TLS handshakes and
// host answers, and a certificate request is a signature over arbitrary
// bytes that the agent process could otherwise ask the oracle for at will.
// Enrolment is a one-off step run by an administrator as the signer's user,
// so it needs no socket purpose and the signer's protocol is unchanged.
//
// This package never reads a key file: BuildCSR takes a crypto.Signer from
// its caller (the signer's own process) and Install takes the signer's
// public half.
//
// It imports no Kubernetes package.
package enrol
