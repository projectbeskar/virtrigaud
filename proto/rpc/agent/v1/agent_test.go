/*
Copyright 2025.

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

package agentv1

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the per-host agent service shape. The service is internal
// to VirtRigaud, but it evolves additively like the provider contract, so a
// change to a message, a field number or a method must fail here until it
// has been reviewed.

const (
	signedBlobMessageName = "provider.v1.SignedBlob"
	agentGoPackage        = "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1;agentv1"
	agentServiceName      = "agent.v1.AgentService"
)

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func blob() *providerv1.SignedBlob {
	body := make([]byte, 256)
	for i := range body {
		body[i] = byte(255 - i)
	}
	return &providerv1.SignedBlob{
		Body:              body,
		Signature:         fill(0x30, 72),
		SignerCertificate: fill(0x31, 512),
		BodyType:          "provider.v1.HostAnswer",
	}
}

func fullMessages() []proto.Message {
	return []proto.Message{
		&HelloRequest{AnswerNonce: fill(0x01, 16), CallerApiVersion: 1},
		&HelloResponse{HostAnswer: blob()},
		&InstallRenewedCertificateRequest{AnswerNonce: fill(0x02, 16), Certificate: fill(0x03, 600), Activate: true},
		&InstallRenewedCertificateResponse{HostAnswer: blob()},
		&AgentStatusRequest{AnswerNonce: fill(0x04, 16)},
		&AgentStatusResponse{HostAnswer: blob()},
	}
}

func TestAgent_MessagesRoundTrip(t *testing.T) {
	for _, m := range fullMessages() {
		r := m.ProtoReflect()
		for i := 0; i < r.Descriptor().Fields().Len(); i++ {
			if fd := r.Descriptor().Fields().Get(i); !r.Has(fd) {
				t.Errorf("%s.%s is not set in the round-trip fixture", r.Descriptor().FullName(), fd.Name())
			}
		}
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %s: %v", r.Descriptor().FullName(), err)
		}
		out := r.New().Interface()
		if err := proto.Unmarshal(raw, out); err != nil {
			t.Fatalf("unmarshal %s: %v", r.Descriptor().FullName(), err)
		}
		if !proto.Equal(m, out) {
			t.Errorf("%s did not round-trip", r.Descriptor().FullName())
		}
	}
}

func TestAgent_HelloResponseBlobBytesExact(t *testing.T) {
	want := blob()
	raw, err := proto.Marshal(&HelloResponse{HostAnswer: blob()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HelloResponse
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b := got.GetHostAnswer()
	if !bytes.Equal(b.GetBody(), want.GetBody()) ||
		!bytes.Equal(b.GetSignature(), want.GetSignature()) ||
		!bytes.Equal(b.GetSignerCertificate(), want.GetSignerCertificate()) ||
		b.GetBodyType() != want.GetBodyType() {
		t.Error("HelloResponse: SignedBlob bytes changed in transit")
	}
}

type fieldSpec struct {
	name     protoreflect.Name
	number   protoreflect.FieldNumber
	kind     protoreflect.Kind
	typeName protoreflect.FullName
}

func checkExactFields(t *testing.T, md protoreflect.MessageDescriptor, want []fieldSpec) {
	t.Helper()
	if md.Fields().Len() != len(want) {
		t.Errorf("%s has %d fields, want %d: update this test only after reviewing the new field", md.FullName(), md.Fields().Len(), len(want))
	}
	for _, w := range want {
		fd := md.Fields().ByName(w.name)
		if fd == nil {
			t.Errorf("%s: field %s missing", md.FullName(), w.name)
			continue
		}
		if fd.Number() != w.number || fd.Kind() != w.kind || fd.Cardinality() == protoreflect.Repeated {
			t.Errorf("%s.%s: number %d kind %v repeated %v, want number %d kind %v singular",
				md.FullName(), w.name, fd.Number(), fd.Kind(), fd.Cardinality() == protoreflect.Repeated, w.number, w.kind)
		}
		var typeName protoreflect.FullName
		if fd.Message() != nil {
			typeName = fd.Message().FullName()
		}
		if typeName != w.typeName {
			t.Errorf("%s.%s: type %q, want %q", md.FullName(), w.name, typeName, w.typeName)
		}
	}
}

func TestAgent_FieldNumbersPinned(t *testing.T) {
	answer := []fieldSpec{{"host_answer", 1, protoreflect.MessageKind, signedBlobMessageName}}
	checkExactFields(t, (&HelloRequest{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"answer_nonce", 1, protoreflect.BytesKind, ""},
		{"caller_api_version", 2, protoreflect.Uint32Kind, ""},
	})
	checkExactFields(t, (&HelloResponse{}).ProtoReflect().Descriptor(), answer)
	checkExactFields(t, (&InstallRenewedCertificateRequest{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"answer_nonce", 1, protoreflect.BytesKind, ""},
		{"certificate", 2, protoreflect.BytesKind, ""},
		{"activate", 3, protoreflect.BoolKind, ""},
	})
	checkExactFields(t, (&InstallRenewedCertificateResponse{}).ProtoReflect().Descriptor(), answer)
	checkExactFields(t, (&AgentStatusRequest{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"answer_nonce", 1, protoreflect.BytesKind, ""},
	})
	checkExactFields(t, (&AgentStatusResponse{}).ProtoReflect().Descriptor(), answer)
	if n := File_agent_v1_agent_proto.Messages().Len(); n != 6 {
		t.Errorf("agent/v1/agent.proto declares %d messages, want 6", n)
	}
	if n := File_agent_v1_agent_proto.Enums().Len(); n != 0 {
		t.Errorf("agent/v1/agent.proto declares %d enums, want 0", n)
	}
}

func TestAgent_ServicePinned(t *testing.T) {
	services := File_agent_v1_agent_proto.Services()
	if services.Len() != 1 {
		t.Fatalf("agent/v1/agent.proto declares %d services, want 1", services.Len())
	}
	sd := services.Get(0)
	if sd.FullName() != agentServiceName {
		t.Errorf("service = %s, want %s", sd.FullName(), agentServiceName)
	}
	want := []struct {
		name, in, out protoreflect.FullName
	}{
		{"Hello", "agent.v1.HelloRequest", "agent.v1.HelloResponse"},
		{"InstallRenewedCertificate", "agent.v1.InstallRenewedCertificateRequest", "agent.v1.InstallRenewedCertificateResponse"},
		{"AgentStatus", "agent.v1.AgentStatusRequest", "agent.v1.AgentStatusResponse"},
	}
	if sd.Methods().Len() != len(want) {
		t.Errorf("%s has %d methods, want %d", sd.FullName(), sd.Methods().Len(), len(want))
	}
	for _, w := range want {
		m := sd.Methods().ByName(protoreflect.Name(w.name))
		if m == nil {
			t.Errorf("%s: method %s missing", sd.FullName(), w.name)
			continue
		}
		if m.IsStreamingClient() || m.IsStreamingServer() {
			t.Errorf("%s.%s is streaming, want unary", sd.FullName(), w.name)
		}
		if m.Input().FullName() != w.in || m.Output().FullName() != w.out {
			t.Errorf("%s.%s: %s -> %s, want %s -> %s", sd.FullName(), w.name, m.Input().FullName(), m.Output().FullName(), w.in, w.out)
		}
	}
	if AgentService_ServiceDesc.ServiceName != string(agentServiceName) {
		t.Errorf("generated service name = %q, want %q", AgentService_ServiceDesc.ServiceName, agentServiceName)
	}
}

func TestAgent_GoPackageAndImports(t *testing.T) {
	fd := File_agent_v1_agent_proto
	opts, ok := fd.Options().(*descriptorpb.FileOptions)
	if !ok {
		t.Fatalf("options are %T", fd.Options())
	}
	if got := opts.GetGoPackage(); got != agentGoPackage {
		t.Errorf("go_package = %q, want %q", got, agentGoPackage)
	}
	if fd.Package() != "agent.v1" {
		t.Errorf("package = %s, want agent.v1", fd.Package())
	}
	if fd.Imports().Len() != 1 || fd.Imports().Get(0).Path() != "provider/v1/host_answer.proto" {
		t.Errorf("agent/v1/agent.proto must import only provider/v1/host_answer.proto")
	}
}
