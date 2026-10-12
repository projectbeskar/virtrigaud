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

package proxmox

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// TestCreateErrors_WireMessagesCarryNoPVEDetail: the PVE endpoint, and the
// text of a PVE error answer, go to the provider log only; the wire message
// names the category. The body of an HTTP 400, which names the rejected
// parameters, is the one exception.
func TestCreateErrors_WireMessagesCarryNoPVEDetail(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		client, err := pveapi.NewClient(&pveapi.Config{Endpoint: "http://127.0.0.1:1", TokenID: "u@pve!t", TokenSecret: "s"})
		require.NoError(t, err)
		p := New()
		p.client = client
		_, err = p.Create(context.Background(), createReq(testOwner(), ""))
		assert.Equal(t, classNothingRemainsProvider, classOf(t, err))
		assert.NotContains(t, err.Error(), "127.0.0.1")
	})
	t.Run("5xx answer", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadGateway, Body: "proxy 10.9.8.7 down"})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
		assert.NotContains(t, err.Error(), "10.9.8.7")
	})
	t.Run("400 names the parameter", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadRequest, Body: "storage: invalid format"})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Contains(t, err.Error(), "storage: invalid format")
	})
}
