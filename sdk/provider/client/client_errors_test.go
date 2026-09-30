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

package client

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// errProviderClient is a providerv1.ProviderClient answering every RPC the
// Client wraps. Like a generated gRPC stub, it returns a response and a nil
// error on success, or a nil response and err when err is set.
type errProviderClient struct {
	providerv1.ProviderClient
	err error
}

// reply answers an RPC with an empty response, or with f.err.
func reply[Resp any](f *errProviderClient) (*Resp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return new(Resp), nil
}

func (f *errProviderClient) Validate(context.Context, *providerv1.ValidateRequest, ...grpc.CallOption) (*providerv1.ValidateResponse, error) {
	return reply[providerv1.ValidateResponse](f)
}

func (f *errProviderClient) Create(context.Context, *providerv1.CreateRequest, ...grpc.CallOption) (*providerv1.CreateResponse, error) {
	return reply[providerv1.CreateResponse](f)
}

func (f *errProviderClient) Delete(context.Context, *providerv1.DeleteRequest, ...grpc.CallOption) (*providerv1.TaskResponse, error) {
	return reply[providerv1.TaskResponse](f)
}

func (f *errProviderClient) Power(context.Context, *providerv1.PowerRequest, ...grpc.CallOption) (*providerv1.TaskResponse, error) {
	return reply[providerv1.TaskResponse](f)
}

func (f *errProviderClient) Reconfigure(context.Context, *providerv1.ReconfigureRequest, ...grpc.CallOption) (*providerv1.TaskResponse, error) {
	return reply[providerv1.TaskResponse](f)
}

func (f *errProviderClient) Describe(context.Context, *providerv1.DescribeRequest, ...grpc.CallOption) (*providerv1.DescribeResponse, error) {
	return reply[providerv1.DescribeResponse](f)
}

// TaskStatus reports the task done, so WaitForTask returns on its first poll.
func (f *errProviderClient) TaskStatus(context.Context, *providerv1.TaskStatusRequest, ...grpc.CallOption) (*providerv1.TaskStatusResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &providerv1.TaskStatusResponse{Done: true}, nil
}

func (f *errProviderClient) SnapshotCreate(context.Context, *providerv1.SnapshotCreateRequest, ...grpc.CallOption) (*providerv1.SnapshotCreateResponse, error) {
	return reply[providerv1.SnapshotCreateResponse](f)
}

func (f *errProviderClient) SnapshotDelete(context.Context, *providerv1.SnapshotDeleteRequest, ...grpc.CallOption) (*providerv1.TaskResponse, error) {
	return reply[providerv1.TaskResponse](f)
}

func (f *errProviderClient) SnapshotRevert(context.Context, *providerv1.SnapshotRevertRequest, ...grpc.CallOption) (*providerv1.TaskResponse, error) {
	return reply[providerv1.TaskResponse](f)
}

func (f *errProviderClient) Clone(context.Context, *providerv1.CloneRequest, ...grpc.CallOption) (*providerv1.CloneResponse, error) {
	return reply[providerv1.CloneResponse](f)
}

func (f *errProviderClient) ImagePrepare(context.Context, *providerv1.ImagePrepareRequest, ...grpc.CallOption) (*providerv1.ImagePrepareResponse, error) {
	return reply[providerv1.ImagePrepareResponse](f)
}

func (f *errProviderClient) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest, ...grpc.CallOption) (*providerv1.GetCapabilitiesResponse, error) {
	return reply[providerv1.GetCapabilitiesResponse](f)
}

func (f *errProviderClient) ListVMs(context.Context, *providerv1.ListVMsRequest, ...grpc.CallOption) (*providerv1.ListVMsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &providerv1.ListVMsResponse{Vms: []*providerv1.VMInfo{{Id: "vm-1"}}}, nil
}

func (f *errProviderClient) TransferOwner(context.Context, *providerv1.TransferOwnerRequest, ...grpc.CallOption) (*providerv1.TransferOwnerResponse, error) {
	return reply[providerv1.TransferOwnerResponse](f)
}

func (f *errProviderClient) ListHosts(context.Context, *providerv1.ListHostsRequest, ...grpc.CallOption) (*providerv1.ListHostsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &providerv1.ListHostsResponse{Hosts: []*providerv1.HostInfo{{Id: "host-1"}}}, nil
}

func (f *errProviderClient) GetHostInfo(context.Context, *providerv1.GetHostInfoRequest, ...grpc.CallOption) (*providerv1.HostInfo, error) {
	return reply[providerv1.HostInfo](f)
}

// rpcCall invokes one Client RPC method and reports whether it returned a
// response, so one table covers methods with different response types.
type rpcCall func(context.Context) (gotResp bool, err error)

// unary adapts a Client method taking a request message to an rpcCall.
func unary[Req, Resp any](rpc func(context.Context, *Req) (*Resp, error)) rpcCall {
	return func(ctx context.Context) (bool, error) {
		resp, err := rpc(ctx, new(Req))
		return resp != nil, err
	}
}

// clientRPCs lists every RPC method of c.
func clientRPCs(c *Client) map[string]rpcCall {
	return map[string]rpcCall{
		"Validate":        unary(c.Validate),
		"Create":          unary(c.Create),
		"Delete":          unary(c.Delete),
		"Power":           unary(c.Power),
		"Reconfigure":     unary(c.Reconfigure),
		"Describe":        unary(c.Describe),
		"TaskStatus":      unary(c.TaskStatus),
		"SnapshotCreate":  unary(c.SnapshotCreate),
		"SnapshotDelete":  unary(c.SnapshotDelete),
		"SnapshotRevert":  unary(c.SnapshotRevert),
		"Clone":           unary(c.Clone),
		"ImagePrepare":    unary(c.ImagePrepare),
		"GetCapabilities": unary(c.GetCapabilities),
		"TransferOwner":   unary(c.TransferOwner),
		"ListVMsResponse": func(ctx context.Context) (bool, error) {
			resp, err := c.ListVMsResponse(ctx)
			return resp != nil, err
		},
		"ListVMs": func(ctx context.Context) (bool, error) {
			vms, err := c.ListVMs(ctx)
			return len(vms) > 0, err
		},
		"ListHosts": func(ctx context.Context) (bool, error) {
			hosts, err := c.ListHosts(ctx)
			return len(hosts) > 0, err
		},
		"GetHostInfo": func(ctx context.Context) (bool, error) {
			host, err := c.GetHostInfo(ctx, "host-1")
			return host != nil, err
		},
	}
}

// TestClientRPCs_CoversEveryMethod keeps clientRPCs complete: a new RPC
// method on Client must be added there so the error tests below cover it.
func TestClientRPCs_CoversEveryMethod(t *testing.T) {
	notRPCs := map[string]bool{"Close": true, "WaitForTask": true}
	rpcs := clientRPCs(&Client{})
	ct := reflect.TypeOf(&Client{})
	for i := range ct.NumMethod() {
		name := ct.Method(i).Name
		if !notRPCs[name] && rpcs[name] == nil {
			t.Errorf("Client.%s is not in clientRPCs", name)
		}
	}
}

// TestClientRPCs_NilErrorOnSuccess is the regression test for the typed-nil
// error: every method used to return errors.FromGRPCError(err) directly, and
// FromGRPCError(nil) is a nil *ProviderError, which is a non-nil error
// interface — so `err != nil` held even when the RPC succeeded.
func TestClientRPCs_NilErrorOnSuccess(t *testing.T) {
	c := &Client{config: &Config{}, client: &errProviderClient{}}
	for name, call := range clientRPCs(c) {
		t.Run(name, func(t *testing.T) {
			gotResp, err := call(context.Background())
			if err != nil {
				t.Fatalf("err = %#v (%T), want a nil error", err, err)
			}
			if !gotResp {
				t.Error("no response returned on success")
			}
		})
	}
}

// TestClientRPCs_ProviderErrorOnFailure: a gRPC status error comes back as a
// *ProviderError carrying the status code and message, with no response.
func TestClientRPCs_ProviderErrorOnFailure(t *testing.T) {
	c := &Client{config: &Config{}, client: &errProviderClient{
		err: status.Error(codes.Unavailable, "provider down"),
	}}
	for name, call := range clientRPCs(c) {
		t.Run(name, func(t *testing.T) {
			gotResp, err := call(context.Background())
			var pe *sdkerrors.ProviderError
			if !errors.As(err, &pe) || pe == nil {
				t.Fatalf("err = %#v (%T), want a non-nil *ProviderError", err, err)
			}
			if pe.Code != codes.Unavailable || pe.Message != "provider down" {
				t.Errorf("ProviderError = {Code: %v, Message: %q}, want {Code: Unavailable, Message: \"provider down\"}", pe.Code, pe.Message)
			}
			if !pe.Retryable {
				t.Error("an Unavailable error must be retryable")
			}
			if gotResp {
				t.Error("a response was returned alongside the error")
			}
		})
	}
}

// TestWaitForTask_CompletedTask: WaitForTask polls TaskStatus, so the
// typed-nil error made it fail every successful poll with
// "failed to check task status: <nil>".
func TestWaitForTask_CompletedTask(t *testing.T) {
	c := &Client{config: &Config{}, client: &errProviderClient{}}
	task := &providerv1.TaskRef{Id: "task-1"}
	if err := c.WaitForTask(context.Background(), task, time.Millisecond); err != nil {
		t.Fatalf("WaitForTask = %v, want nil for a completed task", err)
	}
}

// TestWaitForTask_StatusError: a TaskStatus RPC failure is returned wrapped,
// still reachable as a *ProviderError.
func TestWaitForTask_StatusError(t *testing.T) {
	c := &Client{config: &Config{}, client: &errProviderClient{
		err: status.Error(codes.NotFound, "no such task"),
	}}
	task := &providerv1.TaskRef{Id: "task-1"}
	err := c.WaitForTask(context.Background(), task, time.Millisecond)
	var pe *sdkerrors.ProviderError
	if !errors.As(err, &pe) || pe.Code != codes.NotFound {
		t.Fatalf("WaitForTask = %v, want a wrapped NotFound *ProviderError", err)
	}
}
