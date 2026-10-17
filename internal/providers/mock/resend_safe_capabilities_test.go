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

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestCapabilities_NoResendSafeBackends: the mock serves neither ExportDisk
// nor ImportDisk, so it declares no backend on which a re-send is safe
// (rc2 R6).
func TestCapabilities_NoResendSafeBackends(t *testing.T) {
	caps, err := NewProvider().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Empty(t, caps.GetResendSafeExportBackends())
	assert.Empty(t, caps.GetResendSafeImportBackends())
}
