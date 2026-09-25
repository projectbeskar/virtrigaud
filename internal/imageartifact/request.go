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

package imageartifact

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Mode is how a provider serves an ImagePrepareRequest (ADR-0009 D7).
type Mode int

const (
	// ModeIdentity is a request that carries the VMImage identity and source
	// digest: the provider derives the artifact name (ArtifactName), stamps
	// what it prepares (Stamp) and reuses only on a matching stamp (Decide).
	ModeIdentity Mode = iota + 1
	// ModeLegacy is a request from a manager older than ADR-0009: no identity,
	// a bare target name. It is served with the pre-ADR bare-name naming and
	// reuse by name, without an artifact echo, for this release only, and
	// signalled (SignalLegacyRequest). The next release refuses it with
	// FailedPrecondition.
	ModeLegacy
)

// Request is the provider-side, validated view of an ImagePrepareRequest.
type Request struct {
	// Mode is how the request must be served.
	Mode Mode
	// Image is the VMImage identity (ModeIdentity): a validated UID, namespace
	// and name.
	Image contracts.ObjectIdentity
	// SourceDigest is the validated source digest (ModeIdentity).
	SourceDigest string
	// PreparedBy is the Provider object the request names (ModeIdentity;
	// informational, zero when the request names none).
	PreparedBy contracts.ObjectIdentity
	// LegacyTargetName is the bare target name (ModeLegacy): a DNS-1123
	// subdomain, as every VMImage name is, so it can never be a new-scheme
	// artifact name (those contain '_').
	LegacyTargetName string
}

// ParseRequest decodes and validates req (ADR-0009 D7). It returns a
// codes.InvalidArgument status error for a malformed request:
//
//   - neither an image identity nor a target_name;
//   - an image identity without a uid, or with an invalid uid, namespace or
//     name, or with a target_name (the provider derives the name);
//   - an image identity without a well-formed source_digest;
//   - a provider identity that is set but invalid;
//   - a legacy target_name that is not a DNS-1123 subdomain.
//
// The error is safe to return from the RPC handler as is.
func ParseRequest(req *providerv1.ImagePrepareRequest) (Request, error) {
	image := req.GetImage()
	if image == nil {
		name := req.GetTargetName()
		if name == "" {
			return Request{}, invalidRequest("it carries neither an image identity nor a target_name")
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return Request{}, invalidRequest(fmt.Sprintf("target_name %q: %s", name, strings.Join(errs, "; ")))
		}
		return Request{Mode: ModeLegacy, LegacyTargetName: name}, nil
	}

	if req.GetTargetName() != "" {
		return Request{}, invalidRequest("target_name must be empty when an image identity is set; " +
			"the provider derives the artifact name from the identity")
	}
	parsed := Request{
		Mode:         ModeIdentity,
		Image:        identityFromProto(image),
		SourceDigest: req.GetSourceDigest(),
		PreparedBy:   identityFromProto(req.GetProvider()),
	}
	if err := validateImageIdentity(parsed.Image); err != nil {
		return Request{}, invalidRequest(err.Error())
	}
	if err := ValidateSourceDigest(parsed.SourceDigest); err != nil {
		return Request{}, invalidRequest(err.Error())
	}
	if req.GetProvider() != nil {
		if err := validateProviderIdentity(parsed.PreparedBy); err != nil {
			return Request{}, invalidRequest(err.Error())
		}
	}
	return parsed, nil
}

// identityFromProto converts a wire identity; nil converts to the zero value.
func identityFromProto(o *providerv1.ObjectIdentity) contracts.ObjectIdentity {
	if o == nil {
		return contracts.ObjectIdentity{}
	}
	return contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
}

// validateProviderIdentity returns an error unless the Provider identity p,
// which is written into stamps, is a plausible Kubernetes object identity.
func validateProviderIdentity(p contracts.ObjectIdentity) error {
	if err := validateUID(p.UID); err != nil {
		return fmt.Errorf("provider identity: %w", err)
	}
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return fmt.Errorf("provider identity: namespace %q: %s", p.Namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
		return fmt.Errorf("provider identity: name %q: %s", p.Name, strings.Join(errs, "; "))
	}
	return nil
}

// invalidRequest returns the codes.InvalidArgument error of a malformed
// ImagePrepareRequest.
func invalidRequest(reason string) error {
	return status.Errorf(codes.InvalidArgument, "invalid image prepare request: %s", reason)
}

// ConflictError returns the codes.AlreadyExists error for an artifact at the
// derived name that was not prepared for the requesting VMImage (ADR-0009 D4,
// OutcomeConflict). The message is uniform and names only the requester's own
// artifact: who else prepared it belongs in the provider log only.
func ConflictError(name string) error {
	return status.Errorf(codes.AlreadyExists,
		"a prepared-image artifact named %q exists at this Provider's image location but was not "+
			"prepared for this VMImage; refusing to use or replace it", name)
}

// InProgressError returns the retryable codes.Unavailable error for an
// artifact that is still being prepared for the requesting VMImage (ADR-0009
// D4, OutcomeInProgress).
func InProgressError(name string) error {
	return status.Errorf(codes.Unavailable,
		"the prepared-image artifact %q is still being prepared for this VMImage; retry later", name)
}

// LegacyRequestWarning is the warning logged for every legacy (identity-less)
// ImagePrepare request (ADR-0009 D7).
const LegacyRequestWarning = "deprecated: image prepare without identity from an older manager; " +
	"upgrade the manager; refused from the next release"

// SignalLegacyRequest emits the ADR-0009 D7 deprecation signal for a legacy
// request for targetName served by a provider of providerType ("vsphere",
// "libvirt", "proxmox", "mock"): a WARN log through logger (slog.Default()
// when nil) and one increment of
// virtrigaud_provider_image_prepare_legacy_requests_total{provider_type}.
// Every provider calls it for every legacy request, including one it refuses.
func SignalLegacyRequest(ctx context.Context, logger *slog.Logger, providerType, targetName string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, LegacyRequestWarning, "provider_type", providerType, "target_name", targetName)
	metrics.RecordImagePrepareLegacyRequest(providerType)
}
