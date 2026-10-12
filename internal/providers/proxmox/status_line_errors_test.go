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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// errorForms runs f with the fake answering errors in both forms PVE may use:
// the message in a JSON body, and (as real PVE does) the message in the HTTP
// status line's reason phrase with a {"data":null} body (H-B).
func errorForms(t *testing.T, f func(t *testing.T, p *Provider, srv *pvefake.Server)) {
	for name, inStatusLine := range map[string]bool{"message in body": false, "message in status line": true} {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			srv.ErrorsInStatusLine(inStatusLine)
			f(t, p, srv)
		})
	}
}

func TestErrorForms_MissingVM(t *testing.T) {
	errorForms(t, func(t *testing.T, p *Provider, srv *pvefake.Server) {
		ctx := context.Background()
		srv.PutVM(pvefake.VM{VMID: 900, Name: "moved", Node: "pve2", Status: "stopped"})

		_, err := p.client.GetVMConfig(ctx, "pve", 900)
		assert.ErrorIs(t, err, pveapi.ErrVMNotFound)
		_, err = p.client.DeleteVM(ctx, "pve", 901, true)
		assert.ErrorIs(t, err, pveapi.ErrVMNotFound)

		free, err := p.client.VMIDFree(ctx, 900)
		require.NoError(t, err)
		assert.False(t, free, "400 \"VM 900 already exists\"")

		_, err = p.Describe(ctx, &providerv1.DescribeRequest{Id: "900"})
		require.Error(t, err, "a VM on another node is not gone")
		resp, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: "901"})
		require.NoError(t, err)
		assert.False(t, resp.Exists)
	})
}

func TestErrorForms_AlreadyExistsAndInvalidSpec(t *testing.T) {
	errorForms(t, func(t *testing.T, p *Provider, srv *pvefake.Server) {
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError,
			Body: "unable to create VM: config file already exists", Times: 1,
			Then: putVMAtNewID(t, srv, pvefake.VM{Name: "other", Node: "pve", Status: "running"})})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classNothingRemainsVM, classOf(t, err), "error: %v", err)

		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadRequest,
			Body: "storage: invalid format", Times: 1})
		_, err = p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Contains(t, err.Error(), "storage: invalid format")
	})
}

func TestAPIErrorText(t *testing.T) {
	e := &pveapi.APIError{Operation: "clone VM", StatusCode: 500, Reason: "unable to create VM 105: config file already exists", Body: `{"data":null}`}
	assert.Equal(t, `unable to create VM 105: config file already exists {"data":null}`, e.Text())
	assert.Equal(t, `clone VM failed with status 500: unable to create VM 105: config file already exists {"data":null}`, e.Error())
	assert.Equal(t, "unable to create VM 105: config file already exists", apiErrorDetail(e))
}
