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

package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var testOwner = contracts.ObjectIdentity{UID: "0b8f5e0e-7a53-4f6b-9a0e-1d2c3b4a5f60", Namespace: "team-a", Name: "web"}

// TestConvertCreateRequest_ThreadsOwner pins the manager -> proto mapping of the
// VirtualMachine owner identity (CreateRequest.owner), which a libvirt provider
// uses to refuse binding to another tenant's same-named domain.
func TestConvertCreateRequest_ThreadsOwner(t *testing.T) {
	c := &Client{}

	got, err := c.convertCreateRequest(contracts.CreateRequest{Name: "web", Owner: testOwner})
	require.NoError(t, err)
	require.NotNil(t, got.Owner)
	assert.Equal(t, testOwner.UID, got.Owner.Uid)
	assert.Equal(t, testOwner.Namespace, got.Owner.Namespace)
	assert.Equal(t, testOwner.Name, got.Owner.Name)

	got, err = c.convertCreateRequest(contracts.CreateRequest{Name: "web"})
	require.NoError(t, err)
	assert.Nil(t, got.Owner, "no UID -> no owner on the wire (never a half-filled identity)")
}

// ownerEchoServer records the owner of every Create and answers with err.
type ownerEchoServer struct {
	providerv1.UnimplementedProviderServer
	gotOwner *providerv1.ObjectIdentity
	err      error
}

func (s *ownerEchoServer) Create(_ context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	s.gotOwner = req.GetOwner()
	if s.err != nil {
		return nil, s.err
	}
	return &providerv1.CreateResponse{Id: req.GetName()}, nil
}

// TestCreate_OwnerOnTheWireAndRejectionMapping round-trips Create through an
// in-process gRPC server: the owner arrives intact, and AlreadyExists /
// InvalidArgument answers come back as typed, NON-retryable contract errors.
func TestCreate_OwnerOnTheWireAndRejectionMapping(t *testing.T) {
	srv := &ownerEchoServer{}
	c := newTestClientForVMOps(t, srv, "libvirt", "owner-wire")

	_, err := c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: testOwner})
	require.NoError(t, err)
	require.NotNil(t, srv.gotOwner)
	assert.Equal(t, testOwner.UID, srv.gotOwner.GetUid())
	assert.Equal(t, testOwner.Namespace, srv.gotOwner.GetNamespace())
	assert.Equal(t, testOwner.Name, srv.gotOwner.GetName())

	srv.err = status.Error(codes.AlreadyExists, `libvirt domain "web" already exists on the host and is not owned by this VirtualMachine`)
	_, err = c.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: testOwner})
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "AlreadyExists must map to a Conflict, got %v", err)
	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	assert.False(t, pe.IsRetryable())
	assert.Contains(t, pe.Message, `libvirt domain "web" already exists`)

	srv.err = status.Error(codes.InvalidArgument, `name "12" cannot be used as a libvirt domain name`)
	_, err = c.Create(context.Background(), contracts.CreateRequest{Name: "12", Owner: testOwner})
	assert.True(t, contracts.IsInvalidSpec(err), "InvalidArgument must map to InvalidSpec, got %v", err)
}

// TestMapGRPCError_AlreadyExists pins the typed mapping in isolation.
func TestMapGRPCError_AlreadyExists(t *testing.T) {
	c := &Client{}
	err := c.mapGRPCError("create", status.Error(codes.AlreadyExists, "taken"))
	assert.True(t, contracts.IsConflict(err))
	assert.False(t, contracts.IsNotFound(err))
	assert.Contains(t, err.Error(), "create: taken")
}
