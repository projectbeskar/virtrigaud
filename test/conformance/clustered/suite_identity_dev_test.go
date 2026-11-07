//go:build virtrigaud_dev

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

package clustered_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// TestIdentityCasesWithDevSigner runs the identity.* cases against the
// identity fake with the manager's development signer (operation.DevSigner)
// instead of the test-only one: the grants the development build mints are
// the grants the fake's hosts verify. Only a virtrigaud_dev build has it.
func TestIdentityCasesWithDevSigner(t *testing.T) {
	clustered.Run(t, func(rt clustered.T) clustered.Target {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(rt, err)
		installID, err := operation.NewRandomID()
		require.NoError(rt, err)
		s, err := operation.NewDevSigner(key, installID, nil)
		require.NoError(rt, err)
		tg, err := fake.NewIdentity(fake.IdentityConfig{Anchor: s.Anchor(), Signer: s, Required: true})
		require.NoError(rt, err)
		return tg
	}, clustered.OnlyCases("identity."))
}
