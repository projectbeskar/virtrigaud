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

package operation

import (
	"context"
	"errors"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// DevSignerKeyFileFlagName is the manager flag that names the development
// signer's key file. Only a manager built with the virtrigaud_dev build tag
// accepts it; a release build refuses to start when it is set.
const DevSignerKeyFileFlagName = "dev-operation-signer-key-file"

// ErrNoSigner is returned when an operation grant would have to be signed but
// this build has no signer: a release build (no virtrigaud_dev build tag)
// compiles none in, and refuses to load or use one.
var ErrNoSigner = errors.New("no operation-grant signer is compiled into this build (it needs the " +
	devgate.BuildTag + " build tag)")

// Signature is a signature over a GrantBody's exact bytes and the certificate
// chain that verifies it.
type Signature struct {
	// Signature is the signature over the body bytes by the key of Chain[0].
	Signature []byte
	// Chain holds DER certificates, leaf first: the grant-signing
	// certificate, then its issuer.
	Chain [][]byte
}

// Signer signs operation grants. Implementations never log, return or put in
// an error any key material.
//
// The only implementation is DevSigner, a development-only stand-in compiled
// in only under the virtrigaud_dev build tag. Real custody of the grant
// signing key is out of scope here.
type Signer interface {
	// InstallID is the 16-byte installation identifier the signer's grants
	// carry (GrantBody.install_id).
	InstallID() []byte
	// AuthorityGeneration is the generation of the signing authority
	// (GrantBody.authority_generation), at least 1.
	AuthorityGeneration() uint64
	// Sign signs body, the exact serialised GrantBody, with a certificate
	// chain whose leaf is valid at least until notAfter.
	Sign(ctx context.Context, body []byte, notAfter time.Time) (Signature, error)
}
