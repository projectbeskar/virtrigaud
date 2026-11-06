//go:build !virtrigaud_dev

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

import "fmt"

// SignerCompiled reports whether a signer is compiled into this build: false,
// because it was built without the virtrigaud_dev tag (a release build). The
// manager's transport refuses to send a signed operation when it is false.
const SignerCompiled = false

// LoadSigner loads the development signer from the key file at path. With an
// empty path it returns no signer and no error. This build has no signer, so
// any other path is refused with ErrNoSigner, and the manager exits at
// startup rather than run without the refusal a release build promises.
func LoadSigner(path string) (Signer, error) {
	if path == "" {
		return nil, nil
	}
	return nil, fmt.Errorf("--%s cannot be used with a release build: %w", DevSignerKeyFileFlagName, ErrNoSigner)
}
