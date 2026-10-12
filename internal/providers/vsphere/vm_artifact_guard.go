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

package vsphere

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Per-VM operations never act on image state.
//
// ListVMs keeps templates and prepared images out of adoption
// (list_adoptable.go), but a VirtualMachine can still address one: it was
// adopted before that fix, or an operator converted a template to a VM (where
// adoption took it) and back. The per-VM operations that destroy or change the
// object — Delete, Power, Reconfigure, HardwareUpgrade and snapshot
// create/delete/revert — therefore read config.template and config.extraConfig
// first, and refuse, before changing anything, an object that is a vSphere
// template or carries a prepared-image stamp key (carriesImageStampKey, the
// same test the listing uses). A full Clone is not guarded: cloning FROM a
// template is what templates are for, and a full clone never changes its
// source. A linked Clone of a source without a snapshot first creates one on
// the source (clone-base-*), which changes it, so that step is guarded: a
// linked clone of a template or image artifact is allowed only from a snapshot
// it already has.

const (
	// imageArtifactRefusalMessage is the constant message of the refusal. It
	// names no object, path or stamp value.
	imageArtifactRefusalMessage = "refused: the vSphere object is a template or a VirtRigaud image artifact, not a VM; " +
		"nothing was changed. Detach the VirtualMachine with virtrigaud.io/orphan-on-delete=true and delete it"
	// configUnavailableRefusalMessage is the constant message of a refusal
	// because the object's configuration could not be read: whether it is a
	// template cannot be told.
	configUnavailableRefusalMessage = "refused: the vSphere object's configuration is unavailable, so whether it is a " +
		"template or an image artifact cannot be told; nothing was changed"
)

// imageArtifactStatus is the wire form of the refusal: codes.FailedPrecondition
// (never counted toward the manager's circuit breaker) carrying a
// google.rpc.ErrorInfo{Reason: VM_IS_IMAGE_ARTIFACT}, which the manager maps to
// a non-retryable Conflict marked contracts.ErrVMIsImageArtifact.
func imageArtifactStatus() error {
	st := status.New(codes.FailedPrecondition, imageArtifactRefusalMessage)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: contracts.VMIsImageArtifactReason,
		Domain: contracts.ErrorInfoDomain,
	})
	if err != nil {
		// Unreachable in practice (ErrorInfo always marshals); the bare
		// FailedPrecondition is still a refusal the manager does not retry
		// hot and never counts toward the breaker.
		return st.Err()
	}
	return withInfo.Err()
}

// imageArtifactRefusal decides whether a per-VM operation may act on the
// object whose config is cfg: nil when it is an ordinary VM, the
// VM_IS_IMAGE_ARTIFACT refusal when it is a template or carries a
// prepared-image stamp key, and a plain FailedPrecondition when its
// configuration is unavailable (fail closed).
func imageArtifactRefusal(cfg *types.VirtualMachineConfigInfo) error {
	if cfg == nil {
		return status.Error(codes.FailedPrecondition, configUnavailableRefusalMessage)
	}
	if cfg.Template || carriesImageStampKey(cfg.ExtraConfig) {
		return imageArtifactStatus()
	}
	return nil
}

// refuseImageArtifact reads vm's config.template and config.extraConfig and
// returns imageArtifactRefusal's verdict, or the read error (wrapped; a VM
// that does not exist fails here as it would in the operation itself). op
// names the operation in the log line.
func (p *Provider) refuseImageArtifact(ctx context.Context, vm *object.VirtualMachine, op string) error {
	var vmMo mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{propConfigTemplate, propConfigExtraConfig}, &vmMo); err != nil {
		return fmt.Errorf("read VM configuration before %s: %w", op, err)
	}
	return p.checkNotImageArtifact(vm.Reference().Value, op, vmMo.Config)
}

// checkNotImageArtifact returns imageArtifactRefusal(cfg) for the object id,
// logging a refusal of operation op. It is for an operation that already read
// config.template and config.extraConfig with its own properties.
func (p *Provider) checkNotImageArtifact(id, op string, cfg *types.VirtualMachineConfigInfo) error {
	if err := imageArtifactRefusal(cfg); err != nil {
		p.logger.Warn("Refusing a per-VM operation on a vSphere object that is not an ordinary VM",
			"vm_id", id, "operation", op)
		return err
	}
	return nil
}
