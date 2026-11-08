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

// Package hostidentity holds the pure rules of per-host agent enrolment, used
// only by development builds: the host's URI identity, the enrolment record
// carried in the agent certificate's URI subject alternative names, the key
// pin format, the authorisation policy a certificate request must meet, and
// the validation an issued certificate must pass before its key is pinned.
//
// It does no I/O and imports no Kubernetes or VirtRigaud API package, so the
// manager, the CLI and the per-host agent can share it.
//
// # Enrolment record
//
// The manager draws a 16-byte enrolment identifier for a host and asks for a
// certificate whose URI subject alternative names are exactly:
//
//	spiffe://<trust domain>/virtrigaud/host/<namespace>/<host>
//	urn:virtrigaud:enrolment:<32 hex>
//	urn:virtrigaud:provider:<Provider UID>
//	urn:virtrigaud:install:<32 hex>
//	urn:virtrigaud:mode:identity
//
// The subordinate CA signs them, so whoever holds the certificate can derive
// the enrolment record from it offline, authenticated by the CA (RecordFromURIs).
//
// # Key pins
//
// A pin is the SHA-256 of the DER SubjectPublicKeyInfo of a key, written as 64
// lowercase hexadecimal characters (SPKIPin).
package hostidentity
