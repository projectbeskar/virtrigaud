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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Compile-time assertion that the client can send signed operations.
var _ contracts.OperationSender = (*Client)(nil)

// Timeouts of the signed lifecycle calls: the same as the unsigned ones.
const (
	createCallTimeout = 5 * time.Minute
	deleteCallTimeout = 2 * time.Minute
	powerCallTimeout  = 2 * time.Minute
	// gracefulShutdownTimeoutSeconds is PowerRequest.graceful_timeout_seconds
	// for a graceful shutdown, as the unsigned Power sends it.
	gracefulShutdownTimeoutSeconds = 60
)

// operationKindSpec is how a contracts.OperationKind is sent: its wire kind,
// its RPC's metric name and its timeout.
type operationKindSpec struct {
	kind     providerv1.OperationKind
	metricOp string
	timeout  time.Duration
}

// operationKindSpecs are the kinds the manager can send as signed operations.
var operationKindSpecs = map[contracts.OperationKind]operationKindSpec{
	contracts.OperationKindCreate:           {providerv1.OperationKind_OPERATION_KIND_CREATE, metrics.OpCreate, createCallTimeout},
	contracts.OperationKindDelete:           {providerv1.OperationKind_OPERATION_KIND_DELETE, metrics.OpDelete, deleteCallTimeout},
	contracts.OperationKindPowerOn:          {providerv1.OperationKind_OPERATION_KIND_POWER_ON, metrics.OpPower, powerCallTimeout},
	contracts.OperationKindPowerOff:         {providerv1.OperationKind_OPERATION_KIND_POWER_OFF, metrics.OpPower, powerCallTimeout},
	contracts.OperationKindReboot:           {providerv1.OperationKind_OPERATION_KIND_REBOOT, metrics.OpPower, powerCallTimeout},
	contracts.OperationKindShutdownGraceful: {providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL, metrics.OpPower, powerCallTimeout},
	contracts.OperationKindReconfigure:      {providerv1.OperationKind_OPERATION_KIND_RECONFIGURE, metrics.OpReconfigure, contracts.ReconfigureCallTimeout},
}

// specFor returns how kind is sent, or ErrOperationIdentityNotImplemented.
func specFor(kind contracts.OperationKind) (operationKindSpec, error) {
	spec, ok := operationKindSpecs[kind]
	if !ok {
		return operationKindSpec{}, fmt.Errorf("operation kind %q: %w", kind, contracts.ErrOperationIdentityNotImplemented)
	}
	return spec, nil
}

// NewOperationIdentityClient is NewClient for a Provider that requires
// operation identity: every mutating call goes through BuildOperation and
// SendOperation, signed by signer, and the unsigned mutating methods refuse
// without sending (contracts.ErrOperationIdentityRequired, or
// contracts.ErrOperationIdentityNotImplemented for a call with no signed
// path). It requires TLS with a verified server certificate: a nil tlsConfig
// (plaintext), TLSConfig.Insecure and a prebuilt config with
// InsecureSkipVerify are refused with ErrOperationIdentityNeedsTLS, because
// the request bytes carry secrets and a grant seen on the wire stays valid
// until its not_after. Development builds only: a release build has no signer
// (operation.SignerCompiled is false) and this returns operation.ErrNoSigner,
// as it does for a nil signer. No controller builds one yet.
func NewOperationIdentityClient(ctx context.Context, endpoint, providerType, providerName string,
	cb *resilience.CircuitBreaker, tlsConfig *TLSConfig, signer operation.Signer) (*Client, error) {
	if !operation.SignerCompiled || signer == nil {
		return nil, fmt.Errorf("new operation-identity client for Provider %s: %w", providerName, operation.ErrNoSigner)
	}
	if err := requireVerifiedTLS(tlsConfig); err != nil {
		return nil, fmt.Errorf("new operation-identity client for Provider %s: %w", providerName, err)
	}
	c, err := NewClient(ctx, endpoint, providerType, providerName, cb, tlsConfig)
	if err != nil {
		return nil, err
	}
	c.opSigner = signer
	return c, nil
}

// ErrOperationIdentityNeedsTLS is returned by NewOperationIdentityClient for a
// TLS configuration that would send signed operations in plaintext or without
// verifying the Provider's certificate.
var ErrOperationIdentityNeedsTLS = errors.New("an operation-identity client needs TLS with a verified server certificate")

// requireVerifiedTLS refuses a nil config (plaintext), Insecure, and a
// prebuilt config that skips server verification.
func requireVerifiedTLS(cfg *TLSConfig) error {
	switch {
	case cfg == nil:
		return fmt.Errorf("no TLS configuration (plaintext): %w", ErrOperationIdentityNeedsTLS)
	case cfg.Insecure:
		return fmt.Errorf("TLS configured as insecure: %w", ErrOperationIdentityNeedsTLS)
	case cfg.PrebuiltConfig != nil && cfg.PrebuiltConfig.InsecureSkipVerify:
		return fmt.Errorf("TLS configured to skip server verification: %w", ErrOperationIdentityNeedsTLS)
	}
	return nil
}

// refuseUnsignedCall is nil for a client that is not in operation-identity
// mode (every client but NewOperationIdentityClient's), so the unsigned
// methods behave as before. In that mode it refuses the unsigned call op:
// with ErrOperationIdentityRequired when the call has a signed path, with
// ErrOperationIdentityNotImplemented otherwise. Nothing is sent.
func (c *Client) refuseUnsignedCall(op string, hasSignedPath bool) error {
	if c.opSigner == nil {
		return nil
	}
	if hasSignedPath {
		return fmt.Errorf("%s: %w", op, contracts.ErrOperationIdentityRequired)
	}
	return fmt.Errorf("%s: %w", op, contracts.ErrOperationIdentityNotImplemented)
}

// BuildOperation implements contracts.OperationSender: it builds the request
// message of req's kind (the same fields the unsigned method sends, with
// operation_envelope unset), serialises it exactly once, draws the attempt's
// digest salt, and checks that a grant could be minted for it. It sends
// nothing and needs no signer. A routed request whose host is not
// identity.HostID is refused. The returned bytes can carry secrets and are
// never logged.
func (c *Client) BuildOperation(_ context.Context, identity contracts.OperationAttemptIdentity,
	req contracts.OperationRequest) (contracts.PreparedOperation, error) {
	spec, err := specFor(req.Kind)
	if err != nil {
		return contracts.PreparedOperation{}, fmt.Errorf("build operation: %w", err)
	}
	msg, host, err := c.operationRequestMessage(req)
	if err != nil {
		return contracts.PreparedOperation{}, fmt.Errorf("build %s operation: %w", req.Kind, err)
	}
	if host != "" && host != identity.HostID {
		return contracts.PreparedOperation{}, fmt.Errorf("build %s operation: the request targets host %q, the grant host %q",
			req.Kind, host, identity.HostID)
	}
	reqBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return contracts.PreparedOperation{}, fmt.Errorf("build %s operation: serialise request: %w", req.Kind, err)
	}
	salt, err := operation.NewDigestSalt()
	if err != nil {
		return contracts.PreparedOperation{}, fmt.Errorf("build %s operation: %w", req.Kind, err)
	}
	p := contracts.PreparedOperation{
		Kind:         req.Kind,
		Identity:     copyIdentity(identity),
		RequestBytes: reqBytes,
		DigestSalt:   salt,
	}
	// The installation and generation come from the signer at send time; any
	// valid stand-in checks everything else now.
	installID, generation := make([]byte, operation.IDLen), uint64(1)
	if c.opSigner != nil {
		installID, generation = c.opSigner.InstallID(), c.opSigner.AuthorityGeneration()
	}
	if err := operation.CheckMintRequest(installID, generation, mintRequest(p, spec.kind, time.Now())); err != nil {
		return contracts.PreparedOperation{}, fmt.Errorf("build %s operation: %w", req.Kind, err)
	}
	return p, nil
}

// SendOperation implements contracts.OperationSender: it mints a fresh grant
// over p's exact bytes with the client's signer and sends the envelope on the
// RPC p's kind names, with no other field of the outer request set. A
// re-send of the same PreparedOperation sends the same bytes under a new
// grant. A release build, and a client with no signer, refuse with
// operation.ErrNoSigner and send nothing. The answer is the Provider's
// unsigned claim.
//
// Records virtrigaud_vm_operations_total for the RPC, as the unsigned
// methods do.
func (c *Client) SendOperation(ctx context.Context, p contracts.PreparedOperation) (answer contracts.OperationAnswer, retErr error) {
	if !operation.SignerCompiled || c.opSigner == nil {
		return contracts.OperationAnswer{}, fmt.Errorf("send %s operation: %w", p.Kind, operation.ErrNoSigner)
	}
	spec, err := specFor(p.Kind)
	if err != nil {
		return contracts.OperationAnswer{}, fmt.Errorf("send operation: %w", err)
	}
	grant, err := operation.MintOperationGrant(ctx, c.opSigner, mintRequest(p, spec.kind, time.Now()))
	if err != nil {
		return contracts.OperationAnswer{}, fmt.Errorf("send %s operation: %w", p.Kind, err)
	}
	env := &providerv1.OperationEnvelope{Request: p.RequestBytes, Grant: grant}

	defer c.recordVMOp(spec.metricOp, &retErr)
	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	var task *providerv1.TaskRef
	switch p.Kind {
	case contracts.OperationKindCreate:
		resp, err := c.client.Create(ctx, &providerv1.CreateRequest{OperationEnvelope: env})
		if err != nil {
			return contracts.OperationAnswer{}, c.mapGRPCError("create", err)
		}
		answer.ID, task = resp.GetId(), resp.GetTask()
	case contracts.OperationKindDelete:
		resp, err := c.client.Delete(ctx, &providerv1.DeleteRequest{OperationEnvelope: env})
		if err != nil {
			return contracts.OperationAnswer{}, c.mapGRPCError("delete", err)
		}
		task = resp.GetTask()
	case contracts.OperationKindReconfigure:
		resp, err := c.client.Reconfigure(ctx, &providerv1.ReconfigureRequest{OperationEnvelope: env})
		if err != nil {
			return contracts.OperationAnswer{}, c.mapGRPCError("reconfigure", err)
		}
		answer.RestartRequired, answer.Honest, task = resp.GetRestartRequired(), resp.GetHonestResult(), resp.GetTask()
	default: // the four Power kinds (specFor admitted p.Kind)
		resp, err := c.client.Power(ctx, &providerv1.PowerRequest{OperationEnvelope: env})
		if err != nil {
			return contracts.OperationAnswer{}, c.mapGRPCError("power", err)
		}
		task = resp.GetTask()
	}
	if task != nil {
		answer.TaskRef = task.GetId()
		c.trackTaskStart(task.GetId()) // G7.3 (#129)
	}
	return answer, nil
}

// operationRequestMessage builds the request message of req's kind, as the
// unsigned method builds it, and returns the host it targets (empty for a
// single-host call).
func (c *Client) operationRequestMessage(req contracts.OperationRequest) (proto.Message, string, error) {
	vm := req.VM
	switch req.Kind {
	case contracts.OperationKindCreate:
		m, err := c.convertCreateRequest(req.Spec)
		if err != nil {
			return nil, "", fmt.Errorf("convert create request: %w", err)
		}
		return m, m.GetTargetHostId(), nil
	case contracts.OperationKindDelete:
		return &providerv1.DeleteRequest{Id: vm.ID, TargetHostId: vm.HostID, Owner: vmOwner(vm)}, vm.HostID, nil
	case contracts.OperationKindReconfigure:
		desired, err := json.Marshal(req.Spec)
		if err != nil {
			return nil, "", fmt.Errorf("marshal desired configuration: %w", err)
		}
		return &providerv1.ReconfigureRequest{Id: vm.ID, DesiredJson: string(desired), TargetHostId: vm.HostID,
			Owner: vmOwner(vm)}, vm.HostID, nil
	}
	spec, err := specFor(req.Kind)
	if err != nil {
		return nil, "", err
	}
	row, _ := operation.RowFor(spec.kind) // every spec kind has a row
	m := &providerv1.PowerRequest{Id: vm.ID, Op: row.FixedPowerOp, TargetHostId: vm.HostID, Owner: vmOwner(vm)}
	if req.Kind == contracts.OperationKindShutdownGraceful {
		m.GracefulTimeoutSeconds = gracefulShutdownTimeoutSeconds
	}
	return m, vm.HostID, nil
}

// mintRequest is the MintRequest of p for wire kind kind, issued at now.
func mintRequest(p contracts.PreparedOperation, kind providerv1.OperationKind, now time.Time) operation.MintRequest {
	id := p.Identity
	return operation.MintRequest{
		Identity: operation.Identity{
			ProviderUID: id.ProviderUID,
			HostID:      id.HostID,
			EnrolmentID: id.EnrolmentID,
			Epoch:       operation.Epoch{Counter: id.Epoch.Counter, Holder: id.Epoch.Holder},
			VMUID:       id.VMUID,
			BindingID:   id.BindingID,
			OpID:        id.OpID,
			Kind:        kind,
			Attempt:     id.Attempt,
			MintEpoch:   operation.Epoch{Counter: id.MintEpoch.Counter, Holder: id.MintEpoch.Holder},
		},
		DigestSalt: p.DigestSalt,
		Request:    p.RequestBytes,
		IssuedAt:   now,
	}
}

// copyIdentity deep-copies the byte fields of id, so a PreparedOperation does
// not alias its caller's slices.
func copyIdentity(id contracts.OperationAttemptIdentity) contracts.OperationAttemptIdentity {
	cp := func(b []byte) []byte { return append([]byte(nil), b...) }
	id.EnrolmentID, id.BindingID, id.OpID = cp(id.EnrolmentID), cp(id.BindingID), cp(id.OpID)
	id.Epoch.Holder, id.MintEpoch.Holder = cp(id.Epoch.Holder), cp(id.MintEpoch.Holder)
	return id
}
