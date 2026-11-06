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

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReleaseBuild_HasNoSigner: a release build (no virtrigaud_dev tag) has
// no signer: SignerCompiled is false, and LoadSigner refuses any key file
// with ErrNoSigner without reading it. An empty path is no signer.
func TestReleaseBuild_HasNoSigner(t *testing.T) {
	require.False(t, SignerCompiled, "SignerCompiled must be false without the virtrigaud_dev tag")
	s, err := LoadSigner("")
	require.NoError(t, err)
	assert.Nil(t, s)
	s, err = LoadSigner(t.TempDir() + "/does-not-exist.pem")
	assert.Nil(t, s)
	assert.True(t, errors.Is(err, ErrNoSigner), "%v", err)
	assert.Contains(t, err.Error(), DevSignerKeyFileFlagName)
}
