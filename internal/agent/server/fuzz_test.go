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

package server_test

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fuzzMethods are the methods the fuzz targets send arbitrary requests to.
var fuzzMethods = []string{
	providerv1.Provider_Create_FullMethodName, providerv1.Provider_Delete_FullMethodName,
	providerv1.Provider_Power_FullMethodName, providerv1.Provider_Reconfigure_FullMethodName,
	providerv1.Provider_TaskStatus_FullMethodName, providerv1.Provider_Describe_FullMethodName,
}

// fuzzRequest is the request message of method.
func fuzzRequest(method string) (proto.Message, proto.Message) {
	switch method {
	case providerv1.Provider_Create_FullMethodName:
		return &providerv1.CreateRequest{}, &providerv1.CreateResponse{}
	case providerv1.Provider_Delete_FullMethodName:
		return &providerv1.DeleteRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Power_FullMethodName:
		return &providerv1.PowerRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Reconfigure_FullMethodName:
		return &providerv1.ReconfigureRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_TaskStatus_FullMethodName:
		return &providerv1.TaskStatusRequest{}, &providerv1.TaskStatusResponse{}
	}
	return &providerv1.DescribeRequest{}, &providerv1.DescribeResponse{}
}

// FuzzOperationPathDecoders sends arbitrary request bytes, decoded as the
// request message of a mutating method, TaskStatus or Describe, to a ready
// agent with an operation path. The agent never fails other than with a
// gRPC status, and arbitrary bytes, a re-delivery of a finished operation
// included, never have an effect.
func FuzzOperationPathDecoders(f *testing.F) {
	e := newOpsEnv(f)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", []byte("data")))
	env := &providerv1.OperationEnvelope{Request: o.request, Grant: e.grant(o, time.Now(), 0)}
	signed, _ := proto.Marshal(&providerv1.CreateRequest{OperationEnvelope: env})
	// The signed Create runs to its end first: a re-delivery of it is then
	// answered from its record, and no input may have an effect.
	task, err := e.sendEnvelope(o.method, env, nil)
	if err != nil {
		f.Fatal(err)
	}
	e.waitDone(task)
	f.Add(uint8(0), signed)
	f.Add(uint8(2), []byte{0x2a, 0x00})
	ts, _ := proto.Marshal(&providerv1.TaskStatusRequest{AnswerNonce: nonce(1), OpId: randID(), OpEnrolmentId: randID(), TargetHostId: "host-a"})
	f.Add(uint8(4), ts)
	tr, _ := proto.Marshal(&providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: server.TaskRefPrefix + "00"}})
	f.Add(uint8(4), tr)
	d, _ := proto.Marshal(&providerv1.DescribeRequest{Owner: ownerOf("vm-1"), AnswerNonce: nonce(2), BindingId: randID()})
	f.Add(uint8(5), d)
	conn := e.cc()
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		method := fuzzMethods[int(which)%len(fuzzMethods)]
		req, resp := fuzzRequest(method)
		if err := proto.Unmarshal(data, req); err != nil {
			return
		}
		before := e.exec.total()
		err := conn.Invoke(ctx(t), method, req, resp)
		if err != nil {
			if _, ok := status.FromError(err); !ok || status.Code(err) == codes.Unknown {
				t.Fatalf("%s: not a gRPC status: %v", method, err)
			}
		}
		if e.exec.total() != before {
			t.Fatalf("%s: arbitrary bytes had an effect", method)
		}
	})
}
