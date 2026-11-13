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

package agenttarget

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// taskPrefix starts every task reference the router returns: the host, then
// the agent's own reference.
const taskPrefix = "agent-task/"

// router is the thin in-test router: a clustered Provider that verifies
// nothing and forwards each call to the agent of the host it names.
type router struct {
	providerv1.UnimplementedProviderServer
	t *Target
}

// hostUnavailable is the host-scoped refusal of a host the router does not
// reach.
func hostUnavailable(host string) error {
	st := status.New(codes.Unavailable, "host is unreachable")
	if d, err := st.WithDetails(errInfoOf(contracts.HostUnavailableReason, map[string]string{"host": host})); err == nil {
		return d.Err()
	}
	return st.Err()
}

// hostOf routes a mutating call: by a decoded copy of the request inside the
// envelope when there is one, otherwise by the outer request.
func hostOf(outer proto.Message) (string, error) {
	r := outer.ProtoReflect()
	fd := r.Descriptor().Fields().ByName("operation_envelope")
	routing := outer
	if fd != nil && r.Has(fd) {
		env, ok := r.Get(fd).Message().Interface().(*providerv1.OperationEnvelope)
		if !ok {
			return "", status.Error(codes.InvalidArgument, "operation_envelope has an unexpected type")
		}
		routing = outer.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(env.GetRequest(), routing); err != nil {
			return "", status.Error(codes.InvalidArgument, "operation_envelope.request does not decode")
		}
	}
	h, ok := routing.(interface{ GetTargetHostId() string })
	if !ok || h.GetTargetHostId() == "" {
		return "", status.Error(codes.InvalidArgument, "target_host_id is required")
	}
	return h.GetTargetHostId(), nil
}

// forward sends req on method to host's agent and returns its answer
// unchanged.
func (r *router) forward(ctx context.Context, host, method string, req, resp proto.Message) error {
	h, err := r.t.reach(host)
	if err != nil {
		return err
	}
	return h.Conn().Invoke(ctx, method, req, resp)
}

// mutate forwards a mutating call and encodes the host into its task.
func (r *router) mutate(ctx context.Context, method string, req, resp proto.Message, task func() *providerv1.TaskRef) error {
	host, err := hostOf(req)
	if err != nil {
		return err
	}
	if err := r.forward(ctx, host, method, req, resp); err != nil {
		return err
	}
	if t := task(); t != nil && t.GetId() != "" {
		t.Id = taskPrefix + host + "/" + t.GetId()
		r.t.recordTask(host, strings.TrimPrefix(t.GetId(), taskPrefix+host+"/"))
	}
	return nil
}

// Create implements providerv1.ProviderServer.
func (r *router) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	resp := &providerv1.CreateResponse{}
	if err := r.mutate(ctx, providerv1.Provider_Create_FullMethodName, req, resp, func() *providerv1.TaskRef { return resp.GetTask() }); err != nil {
		return nil, err
	}
	return resp, nil
}

// Delete implements providerv1.ProviderServer.
func (r *router) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	resp := &providerv1.TaskResponse{}
	if err := r.mutate(ctx, providerv1.Provider_Delete_FullMethodName, req, resp, func() *providerv1.TaskRef { return resp.GetTask() }); err != nil {
		return nil, err
	}
	return resp, nil
}

// Power implements providerv1.ProviderServer.
func (r *router) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	resp := &providerv1.TaskResponse{}
	if err := r.mutate(ctx, providerv1.Provider_Power_FullMethodName, req, resp, func() *providerv1.TaskRef { return resp.GetTask() }); err != nil {
		return nil, err
	}
	return resp, nil
}

// Reconfigure implements providerv1.ProviderServer.
func (r *router) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	resp := &providerv1.TaskResponse{}
	if err := r.mutate(ctx, providerv1.Provider_Reconfigure_FullMethodName, req, resp, func() *providerv1.TaskRef { return resp.GetTask() }); err != nil {
		return nil, err
	}
	return resp, nil
}

// TaskStatus implements providerv1.ProviderServer: a request with
// answer_nonce is routed by target_host_id and forwarded unchanged; any
// other by its host-encoded task reference.
func (r *router) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	resp := &providerv1.TaskStatusResponse{}
	if len(req.GetAnswerNonce()) != 0 {
		if req.GetTask() != nil {
			return nil, status.Error(codes.InvalidArgument, "a request with answer_nonce names no task")
		}
		if err := r.forward(ctx, req.GetTargetHostId(), providerv1.Provider_TaskStatus_FullMethodName, req, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
	rest, ok := strings.CutPrefix(req.GetTask().GetId(), taskPrefix)
	host, inner, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || inner == "" {
		return nil, status.Error(codes.InvalidArgument, "task reference is not one this provider issued")
	}
	if err := r.forward(ctx, host, providerv1.Provider_TaskStatus_FullMethodName,
		&providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: inner}}, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Describe implements providerv1.ProviderServer: routed by target_host_id
// and forwarded unchanged.
func (r *router) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	resp := &providerv1.DescribeResponse{}
	if err := r.forward(ctx, req.GetTargetHostId(), providerv1.Provider_Describe_FullMethodName, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// GetCapabilities implements providerv1.ProviderServer: the router fronts
// hosts that verify operation grants and requires one on every mutating
// call.
func (r *router) GetCapabilities(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{SupportsOperationIdentity: true, RequiresOperationIdentity: true}, nil
}
