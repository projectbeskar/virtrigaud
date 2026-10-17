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

package libvirt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestExportSourceOn_MissingDomainIsNotFound: the source of an s3 or nfs
// export is read on a single host and on a routed one (exportDiskRouted) by
// the same function. A domain that is not on the host is NotFound in both; a
// routed export maps it to codes.NotFound as it maps every NotFound
// (routedRPCError), and a definition that cannot be read stays retryable.
func TestExportSourceOn_MissingDomainIsNotFound(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	vp := fx.p.virshProvider

	_, _, err := exportSourceOn(context.Background(), vp, byName("gone"), "")
	require.Error(t, err)
	assert.True(t, contracts.IsNotFound(err), "%v", err)

	// The routed export's wire mapping of the NotFound.
	assert.Equal(t, codes.NotFound, status.Code(routedRPCError("export disk", err)))

	fx.script("single", "fail-dumpxml", "")
	_, _, err = exportSourceOn(context.Background(), vp, byName(scdDomain), "")
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "an unreadable definition is not a missing domain: %v", err)
}
