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
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// A single-host Describe of a domain virsh does not have (VIR_ERR_NO_DOMAIN)
// is a definite NotFound, on the wire too; any other failure is unchanged.
func TestDescribe_SingleHostAbsentDomainIsNotFound(t *testing.T) {
	newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	p := &Provider{virshProvider: localHostVP("single")}

	_, err := p.Describe(context.Background(), contracts.VMRef{ID: "team-a.absent"})
	require.Error(t, err)
	assert.True(t, contracts.IsNotFound(err), "an absent domain is a definite NotFound: %v", err)

	_, err = NewServer(p).Describe(context.Background(), &providerv1.DescribeRequest{Id: "team-a.absent"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))

	resp, err := p.Describe(context.Background(), contracts.VMRef{ID: opsDomainName})
	require.NoError(t, err, "an existing domain is described as before")
	assert.True(t, resp.Exists)
}

func TestDescribe_SingleHostOtherFailureIsUnchanged(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{
		"single": {opsDomainName: routingDomainXML(opsDomainName, contracts.ObjectIdentity{})},
	})
	fx.script("single", "nolibvirtd", "")
	p := &Provider{virshProvider: localHostVP("single")}
	_, err := p.Describe(context.Background(), contracts.VMRef{ID: opsDomainName})
	require.Error(t, err)
	assert.False(t, contracts.IsNotFound(err), "a connection failure is not a NotFound")
	assert.True(t, contracts.IsRetryable(err))

	_, err = NewServer(p).Describe(context.Background(), &providerv1.DescribeRequest{Id: opsDomainName})
	require.Error(t, err)
	assert.Equal(t, codes.Unknown, status.Code(err), "the historical wire form")
	assert.Contains(t, err.Error(), "failed to describe VM")
}

func TestVirshNoDomain(t *testing.T) {
	assert.True(t, virshNoDomain(&VirshError{Stderr: "error: failed to get domain 'web'"}))
	assert.True(t, virshNoDomain(&VirshError{Stderr: "error: Domain not found: no domain with matching name 'web'"}))
	assert.False(t, virshNoDomain(&VirshError{Stderr: "error: failed to connect to the hypervisor"}))
	assert.False(t, virshNoDomain(stderrors.New("failed to get domain")), "only a virsh command's stderr counts")
}
