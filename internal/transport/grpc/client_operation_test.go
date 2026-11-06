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
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// opRecorderServer records every mutating request as it arrived on the wire,
// keyed by RPC, and answers with fixed responses.
type opRecorderServer struct {
	providerv1.UnimplementedProviderServer
	mu   sync.Mutex
	reqs map[string][]proto.Message
}

func (s *opRecorderServer) record(name string, m proto.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqs == nil {
		s.reqs = map[string][]proto.Message{}
	}
	s.reqs[name] = append(s.reqs[name], proto.Clone(m))
}

func (s *opRecorderServer) all() map[string][]proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]proto.Message{}
	for k, v := range s.reqs {
		out[k] = append([]proto.Message(nil), v...)
	}
	return out
}

func (s *opRecorderServer) last(t *testing.T, name string) proto.Message {
	t.Helper()
	got := s.all()[name]
	require.NotEmpty(t, got, "%s never reached the server", name)
	return got[len(got)-1]
}

func (s *opRecorderServer) Create(_ context.Context, r *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	s.record(providerv1.Provider_Create_FullMethodName, r)
	return &providerv1.CreateResponse{Id: "vm-id-1", Task: &providerv1.TaskRef{Id: "task-create"}}, nil
}

func (s *opRecorderServer) Delete(_ context.Context, r *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	s.record(providerv1.Provider_Delete_FullMethodName, r)
	return &providerv1.TaskResponse{Task: &providerv1.TaskRef{Id: "task-delete"}}, nil
}

func (s *opRecorderServer) Power(_ context.Context, r *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	s.record(providerv1.Provider_Power_FullMethodName, r)
	return &providerv1.TaskResponse{}, nil
}

func (s *opRecorderServer) Reconfigure(_ context.Context, r *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	s.record(providerv1.Provider_Reconfigure_FullMethodName, r)
	return &providerv1.TaskResponse{RestartRequired: true, HonestResult: true}, nil
}

// stubSigner is a Signer that never signs: it puts a client in
// operation-identity mode in tests that must send nothing.
type stubSigner struct{}

func (stubSigner) InstallID() []byte           { return make([]byte, operation.IDLen) }
func (stubSigner) AuthorityGeneration() uint64 { return 1 }
func (stubSigner) Sign(context.Context, []byte, time.Time) (operation.Signature, error) {
	return operation.Signature{}, errors.New("stubSigner does not sign")
}

// testVerifiedTLS is a TLS configuration that verifies the server (the
// connection is lazy; these tests never dial it).
func testVerifiedTLS() *TLSConfig {
	return &TLSConfig{PrebuiltConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "provider.test"}}
}

// opTestVM is the routed VM of these tests.
var opTestVM = contracts.VMRef{ID: "vm-1", HostID: "host-a",
	Owner: contracts.ObjectIdentity{UID: "vm-uid-1", Namespace: "tenant", Name: "vm"}}

// opTestIdentity is a valid attempt identity for opTestVM.
func opTestIdentity() contracts.OperationAttemptIdentity {
	b := func(v byte) []byte {
		out := make([]byte, operation.IDLen)
		for i := range out {
			out[i] = v
		}
		return out
	}
	return contracts.OperationAttemptIdentity{
		ProviderUID: "provider-uid-1",
		HostID:      opTestVM.HostID,
		EnrolmentID: b(0xE1),
		Epoch:       contracts.OperationEpoch{Counter: 1, Holder: b(0xC1)},
		VMUID:       opTestVM.Owner.UID,
		BindingID:   b(0xB1),
		OpID:        b(0x01),
		Attempt:     1,
		MintEpoch:   contracts.OperationEpoch{Counter: 1, Holder: b(0xC1)},
	}
}

// opTestRequest is the request of kind for opTestVM.
func opTestRequest(kind contracts.OperationKind) contracts.OperationRequest {
	spec := contracts.CreateRequest{Name: "vm", TargetHostID: opTestVM.HostID, Owner: opTestVM.Owner,
		UserData: &contracts.UserData{CloudInitData: "#cloud-config\npassword: op-secret"}}
	return contracts.OperationRequest{Kind: kind, VM: opTestVM, Spec: spec}
}

// allOperationKinds are the kinds the manager sends as signed operations.
var allOperationKinds = []contracts.OperationKind{
	contracts.OperationKindCreate, contracts.OperationKindDelete, contracts.OperationKindPowerOn,
	contracts.OperationKindPowerOff, contracts.OperationKindReboot, contracts.OperationKindShutdownGraceful,
	contracts.OperationKindReconfigure,
}

// TestOperationIdentityMode_UnsignedMethodsRefuse: a client in
// operation-identity mode refuses every unsigned mutating method and sends
// nothing: ErrOperationIdentityRequired for the lifecycle calls that have a
// signed path, ErrOperationIdentityNotImplemented for the others.
func TestOperationIdentityMode_UnsignedMethodsRefuse(t *testing.T) {
	srv := &opRecorderServer{}
	cli := newTestClientForVMOps(t, srv, "cloudhypervisor", "identity")
	cli.opSigner = stubSigner{}
	ctx := context.Background()
	required := map[string]error{}
	notImplemented := map[string]error{}
	_, required["Create"] = cli.Create(ctx, contracts.CreateRequest{Name: "vm"})
	_, required["Delete"] = cli.Delete(ctx, opTestVM)
	_, required["Power"] = cli.Power(ctx, opTestVM, contracts.PowerOpOn)
	_, required["Reconfigure"] = cli.Reconfigure(ctx, opTestVM, contracts.CreateRequest{})
	_, notImplemented["Clone"] = cli.Clone(ctx, contracts.CloneRequest{Source: opTestVM})
	_, notImplemented["PrepareImage"] = cli.PrepareImage(ctx, contracts.ImagePrepareRequest{})
	_, notImplemented["SnapshotCreate"] = cli.SnapshotCreate(ctx, contracts.SnapshotCreateRequest{VM: opTestVM})
	_, notImplemented["SnapshotDelete"] = cli.SnapshotDelete(ctx, opTestVM, "s")
	_, notImplemented["SnapshotRevert"] = cli.SnapshotRevert(ctx, opTestVM, "s")
	_, notImplemented["ExportDisk"] = cli.ExportDisk(ctx, contracts.ExportDiskRequest{VM: opTestVM})
	_, notImplemented["ImportDisk"] = cli.ImportDisk(ctx, contracts.ImportDiskRequest{})
	notImplemented["TransferOwner"] = cli.TransferOwner(ctx, contracts.TransferOwnerRequest{})
	_, notImplemented["DeleteStagedDisk"] = cli.DeleteStagedDisk(ctx, contracts.StagedDiskDeleteRequest{})
	_, notImplemented["DeleteLandedDisk"] = cli.DeleteLandedDisk(ctx, contracts.LandedDiskDeleteRequest{})
	for name, err := range required {
		assert.ErrorIs(t, err, contracts.ErrOperationIdentityRequired, name)
	}
	for name, err := range notImplemented {
		assert.ErrorIs(t, err, contracts.ErrOperationIdentityNotImplemented, name)
	}
	assert.Empty(t, srv.all(), "an unsigned call reached the Provider")
}

// TestBuildOperation_SameRequestAsTheUnsignedCall: for every kind, the bytes
// BuildOperation returns decode to exactly the request the unsigned method
// sends for the same input, with operation_envelope unset.
func TestBuildOperation_SameRequestAsTheUnsignedCall(t *testing.T) {
	srv := &opRecorderServer{}
	plain := newTestClientForVMOps(t, srv, "libvirt", "plain")
	ctx := context.Background()
	unsigned := map[contracts.OperationKind]func() (string, proto.Message){
		contracts.OperationKindCreate: func() (string, proto.Message) {
			_, err := plain.Create(ctx, opTestRequest(contracts.OperationKindCreate).Spec)
			require.NoError(t, err)
			return providerv1.Provider_Create_FullMethodName, &providerv1.CreateRequest{}
		},
		contracts.OperationKindDelete: func() (string, proto.Message) {
			_, err := plain.Delete(ctx, opTestVM)
			require.NoError(t, err)
			return providerv1.Provider_Delete_FullMethodName, &providerv1.DeleteRequest{}
		},
		contracts.OperationKindReconfigure: func() (string, proto.Message) {
			_, err := plain.Reconfigure(ctx, opTestVM, opTestRequest(contracts.OperationKindReconfigure).Spec)
			require.NoError(t, err)
			return providerv1.Provider_Reconfigure_FullMethodName, &providerv1.ReconfigureRequest{}
		},
	}
	for kind, op := range map[contracts.OperationKind]contracts.PowerOp{
		contracts.OperationKindPowerOn: contracts.PowerOpOn, contracts.OperationKindPowerOff: contracts.PowerOpOff,
		contracts.OperationKindReboot: contracts.PowerOpReboot, contracts.OperationKindShutdownGraceful: contracts.PowerOpShutdownGraceful,
	} {
		unsigned[kind] = func() (string, proto.Message) {
			_, err := plain.Power(ctx, opTestVM, op)
			require.NoError(t, err)
			return providerv1.Provider_Power_FullMethodName, &providerv1.PowerRequest{}
		}
	}
	for _, kind := range allOperationKinds {
		t.Run(string(kind), func(t *testing.T) {
			p, err := plain.BuildOperation(ctx, opTestIdentity(), opTestRequest(kind))
			require.NoError(t, err)
			assert.Equal(t, kind, p.Kind)
			assert.Len(t, p.DigestSalt, operation.SaltLen)
			method, decoded := unsigned[kind]()
			require.NoError(t, proto.Unmarshal(p.RequestBytes, decoded))
			assert.True(t, proto.Equal(srv.last(t, method), decoded), "%s: built %v, unsigned sent %v", kind, decoded, srv.last(t, method))
		})
	}
}

// TestBuildOperation_Refusals: a kind with no signed path, a request for
// another host than the grant's, and an identity the minter would refuse are
// refused before anything is persisted or sent.
func TestBuildOperation_Refusals(t *testing.T) {
	srv := &opRecorderServer{}
	cli := newTestClientForVMOps(t, srv, "cloudhypervisor", "identity")
	ctx := context.Background()

	_, err := cli.BuildOperation(ctx, opTestIdentity(), contracts.OperationRequest{Kind: "Clone"})
	assert.ErrorIs(t, err, contracts.ErrOperationIdentityNotImplemented)

	otherHost := opTestRequest(contracts.OperationKindDelete)
	otherHost.VM.HostID = "host-b"
	_, err = cli.BuildOperation(ctx, opTestIdentity(), otherHost)
	assert.Error(t, err)

	for name, edit := range map[string]func(id *contracts.OperationAttemptIdentity){
		"attempt 0":       func(id *contracts.OperationAttemptIdentity) { id.Attempt = 0 },
		"epoch counter 0": func(id *contracts.OperationAttemptIdentity) { id.Epoch.Counter = 0 },
		"op id missing":   func(id *contracts.OperationAttemptIdentity) { id.OpID = nil },
		"no provider uid": func(id *contracts.OperationAttemptIdentity) { id.ProviderUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			id := opTestIdentity()
			edit(&id)
			_, err := cli.BuildOperation(ctx, id, opTestRequest(contracts.OperationKindDelete))
			assert.Error(t, err)
		})
	}
	assert.Empty(t, srv.all())
}

// TestPreparedOperation_StringRedacts: formatting a PreparedOperation never
// prints the request bytes or the salt.
func TestPreparedOperation_StringRedacts(t *testing.T) {
	cli := newTestClientForVMOps(t, &opRecorderServer{}, "cloudhypervisor", "identity")
	p, err := cli.BuildOperation(context.Background(), opTestIdentity(), opTestRequest(contracts.OperationKindCreate))
	require.NoError(t, err)
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), p.String()} {
		assert.NotContains(t, s, "op-secret")
		assert.Contains(t, s, "redacted")
	}
}
