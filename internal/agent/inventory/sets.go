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

package inventory

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Versions of the inventory content.
const (
	// SupportedVersion is the highest inventory version this agent
	// implements.
	SupportedVersion uint32 = 1
)

// Bounds of a report. A collection that would exceed one does not truncate:
// the snapshot is OVER_LIMIT and lists nothing.
const (
	// MaxComponents bounds the components.
	MaxComponents = 32
	// MaxFilesPerComponent bounds one component's files.
	MaxFilesPerComponent = 8
	// MaxFiles bounds the files of all components.
	MaxFiles = 64
	// MaxFeatures bounds the feature probes.
	MaxFeatures = 32
	// MaxExtensions bounds the unexpected extensions.
	MaxExtensions = 16
	// MaxInventoryBytes bounds the encoded HostInventory.
	MaxInventoryBytes = 32 << 10
)

// Reasons of a HostInventory.state_reason, by state, for inventory version 1.
const (
	// ReasonTooManyComponents: more components than MaxComponents, or more
	// manifest directory entries than MaxManifestEntries.
	ReasonTooManyComponents = "too_many_components"
	// ReasonTooManyFiles: more files than MaxFilesPerComponent or MaxFiles.
	ReasonTooManyFiles = "too_many_files"
	// ReasonTooManyFeatures: more feature probes than MaxFeatures.
	ReasonTooManyFeatures = "too_many_features"
	// ReasonTooManyExtensions: more unexpected extensions than MaxExtensions.
	ReasonTooManyExtensions = "too_many_extensions"
	// ReasonInventoryTooLarge: the encoded report exceeds MaxInventoryBytes.
	ReasonInventoryTooLarge = "inventory_too_large"
	// ReasonIOError: a file could not be read.
	ReasonIOError = "io_error"
	// ReasonManifestDirUnreadable: a manifest directory could not be read.
	ReasonManifestDirUnreadable = "manifest_dir_unreadable"
	// ReasonProbeFailed: the collection could not finish (its budget ended,
	// or it produced a report that breaks a field rule).
	ReasonProbeFailed = "probe_failed"
)

// Reasons of a HostComponent.state_reason for inventory version 1.
const (
	// ReasonNotInstalled: MISSING, the component's files or artifact are not
	// on the host.
	ReasonNotInstalled = "not_installed"
	// ReasonNotDeclared: MISSING, the catalog requires the component and no
	// manifest declares it.
	ReasonNotDeclared = "not_declared"
	// ReasonVersionOutOfRange: INCOMPATIBLE, the observed version is outside
	// the required range.
	ReasonVersionOutOfRange = "version_out_of_range"
	// ReasonVersionUnreadable: INCOMPATIBLE, no version source gave a
	// version.
	ReasonVersionUnreadable = "version_unreadable"
	// ReasonUnexpectedExtension: UNHEALTHY, a directory extension overlays
	// the component's files.
	ReasonUnexpectedExtension = "unexpected_extension"
	// ReasonArtifactDigestMismatch: UNHEALTHY, the artifact's digest differs
	// from the manifest's.
	ReasonArtifactDigestMismatch = "artifact_digest_mismatch"
	// ReasonFileDigestMismatch: UNHEALTHY, a file's digest differs from the
	// manifest's.
	ReasonFileDigestMismatch = "file_digest_mismatch"
	// ReasonSocketUnanswered: UNHEALTHY, a health socket gave no valid
	// answer. No catalog component has a health socket yet.
	ReasonSocketUnanswered = "socket_unanswered"
	// ReasonSelfTestFailed: UNHEALTHY, a self-test failed. The agent runs no
	// self-test yet.
	ReasonSelfTestFailed = "self_test_failed"
	// ReasonDigestLimitExceeded: UNHEALTHY, hashing would read more than the
	// per-file or per-collection limit.
	ReasonDigestLimitExceeded = "digest_limit_exceeded"
	// ReasonNotContentAddressed: UNHEALTHY, the files are not in a
	// content-addressed copy. The agent does not check this yet.
	ReasonNotContentAddressed = "not_content_addressed"
	// ReasonPresentUndeclared: UNMANIFESTED.
	ReasonPresentUndeclared = "present_undeclared"
	// ReasonParseError: MANIFEST_INVALID, the manifest is not valid.
	ReasonParseError = "parse_error"
	// ReasonUnsupportedDigest: MANIFEST_INVALID, a digest is not
	// "sha256:<64 hex digits>".
	ReasonUnsupportedDigest = "unsupported_digest"
	// ReasonDuplicateDeclaration: MANIFEST_INVALID, two manifests declare
	// the component.
	ReasonDuplicateDeclaration = "duplicate_declaration"
	// ReasonVersionTooLong: MANIFEST_INVALID, the declared version is longer
	// than 64 bytes.
	ReasonVersionTooLong = "version_too_long"
	// ReasonInvalidName: MANIFEST_INVALID, the component or artifact name
	// breaks the name rule.
	ReasonInvalidName = "invalid_name"
	// ReasonTooLarge: MANIFEST_INVALID, the manifest file is larger than
	// MaxManifestBytes.
	ReasonTooLarge = "too_large"
	// ReasonUnknownComponent: MANIFEST_INVALID, the component is not in the
	// agent's catalog.
	ReasonUnknownComponent = "unknown_component"
)

// Feature probe names and reasons of inventory version 1.
const (
	// FeatureCgroupV2: a unified cgroup v2 hierarchy is mounted.
	FeatureCgroupV2 = "cgroup_v2"
	// FeatureCloneIntoCgroup: clone3 can start a process in a given cgroup.
	FeatureCloneIntoCgroup = "clone_into_cgroup"
	// FeaturePidfd: pidfd_open is available.
	FeaturePidfd = "pidfd"
	// FeaturePidfsHandle: file handles of process file descriptors work.
	FeaturePidfsHandle = "pidfs_handle"
	// FeaturePidfdInfoExit: a process file descriptor reports the exit
	// status.
	FeaturePidfdInfoExit = "pidfd_info_exit"
	// FeatureInitialPIDNamespace: the agent runs in the initial PID
	// namespace.
	FeatureInitialPIDNamespace = "initial_pid_namespace"
	// FeatureInitialCgroupNamespace: the agent runs in the initial cgroup
	// namespace.
	FeatureInitialCgroupNamespace = "initial_cgroup_namespace"
	// FeatureInitialTimeNamespace: the agent runs in the initial time
	// namespace.
	FeatureInitialTimeNamespace = "initial_time_namespace"
	// FeatureHypervisorPauseResume: the hypervisor can pause and resume a
	// guest. It needs a self-test the agent does not run, so it is reported
	// as ERROR with not_evaluated.
	FeatureHypervisorPauseResume = "hypervisor_pause_resume"
	// FeatureKVM: /dev/kvm is the KVM device and the agent can open it.
	FeatureKVM = "kvm"
	// FeatureTun: /dev/net/tun is the TUN device and the agent can open it.
	FeatureTun = "tun"
	// FeatureSecureBoot: the firmware booted with Secure Boot enabled.
	FeatureSecureBoot = "secure_boot"
	// FeatureMeasuredBoot: a TPM firmware event log is present.
	FeatureMeasuredBoot = "measured_boot"
	// FeatureExtensionImagePolicySigned: system extension images must be
	// signed. The agent cannot read the policy as data, so it is reported as
	// ERROR with not_evaluated.
	FeatureExtensionImagePolicySigned = "extension_image_policy_signed"

	// FeatureReasonProbeFailed: the probe could not decide.
	FeatureReasonProbeFailed = "probe_failed"
	// FeatureReasonProbeTimeout: the probe did not finish in time.
	FeatureReasonProbeTimeout = "probe_timeout"
	// FeatureReasonPermissionDenied: the probe was refused by permission.
	FeatureReasonPermissionDenied = "permission_denied"
	// FeatureReasonNotEvaluated: the agent does not evaluate the probe (it
	// cannot be decided as a data read).
	FeatureReasonNotEvaluated = "not_evaluated"
)

// valueSet is the closed value sets of one inventory version (the verifier's
// table; pinned by test).
type valueSet struct {
	overLimit   []string
	unreadable  []string
	component   map[providerv1.ComponentState][]string
	features    []string
	featReasons []string
	// maxState, maxComponentState, maxOrigin, maxResult, maxTier, maxKind are
	// the highest enum values of the version.
	maxState, maxComponentState, maxOrigin, maxResult, maxTier, maxKind int32
}

// valueSets maps an inventory version to its value sets.
var valueSets = map[uint32]*valueSet{
	1: {
		overLimit:  []string{ReasonTooManyComponents, ReasonTooManyFiles, ReasonTooManyFeatures, ReasonTooManyExtensions, ReasonInventoryTooLarge},
		unreadable: []string{ReasonIOError, ReasonManifestDirUnreadable, ReasonProbeFailed},
		component: map[providerv1.ComponentState][]string{
			providerv1.ComponentState_COMPONENT_STATE_MISSING:      {ReasonNotInstalled, ReasonNotDeclared},
			providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE: {ReasonVersionOutOfRange, ReasonVersionUnreadable},
			providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY: {ReasonUnexpectedExtension, ReasonArtifactDigestMismatch,
				ReasonFileDigestMismatch, ReasonSocketUnanswered, ReasonSelfTestFailed, ReasonDigestLimitExceeded, ReasonNotContentAddressed},
			providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED: {ReasonPresentUndeclared},
			providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID: {ReasonParseError, ReasonUnsupportedDigest, ReasonDuplicateDeclaration,
				ReasonVersionTooLong, ReasonInvalidName, ReasonTooLarge, ReasonUnknownComponent},
		},
		features: []string{FeatureCgroupV2, FeatureCloneIntoCgroup, FeaturePidfd, FeaturePidfsHandle, FeaturePidfdInfoExit,
			FeatureInitialPIDNamespace, FeatureInitialCgroupNamespace, FeatureInitialTimeNamespace, FeatureHypervisorPauseResume,
			FeatureKVM, FeatureTun, FeatureSecureBoot, FeatureMeasuredBoot, FeatureExtensionImagePolicySigned},
		featReasons:       []string{FeatureReasonProbeFailed, FeatureReasonProbeTimeout, FeatureReasonPermissionDenied, FeatureReasonNotEvaluated},
		maxState:          4,
		maxComponentState: 6,
		maxOrigin:         3,
		maxResult:         3,
		maxTier:           4,
		maxKind:           3,
	},
}

// Field shapes: the length and character rules of names, versions, ranges and
// sources.
var (
	componentNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	fileNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	versionRE       = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~_-]{0,63}$`)
	rangeRE         = regexp.MustCompile(`^[0-9A-Za-z.+~_<>=, -]{1,64}$`)
	sourceRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// invalidManifestPrefix starts the name of a component whose manifest cannot
// be used and does not name a catalog component.
const invalidManifestPrefix = "manifest-"

// encodedLimit is the size bound Validate applies; only tests lower it.
var encodedLimit atomic.Int64

func init() { encodedLimit.Store(MaxInventoryBytes) }

// ErrInvalid is wrapped by every violation of an inventory field rule.
var ErrInvalid = errors.New("invalid host inventory")

// ErrTooLarge is wrapped by the violation of the encoded size bound. The
// caller answers with OverLimit instead.
var ErrTooLarge = errors.New("host inventory exceeds the size bound")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate enforces the inventory field rules (version binding, ordering,
// bounds, state consistency, closed value sets) on inv, which answers a
// request whose inventory_version was requested. A report that fails is never
// signed. The error wraps ErrInvalid (ErrTooLarge as well for the size
// bound).
func Validate(inv *providerv1.HostInventory, requested uint32) error {
	if inv == nil {
		return invalidf("no inventory")
	}
	if requested == 0 {
		return invalidf("an inventory for a request that asked for none")
	}
	if inv.GetRequestedVersion() != requested {
		return invalidf("requested_version %d is not the request's %d", inv.GetRequestedVersion(), requested)
	}
	if inv.GetSupportedVersion() == 0 || inv.GetSupportedVersion() > SupportedVersion {
		return invalidf("supported_version %d", inv.GetSupportedVersion())
	}
	if want := min(requested, inv.GetSupportedVersion()); inv.GetInventoryVersion() != want {
		return invalidf("inventory_version %d is not min(requested, supported) = %d", inv.GetInventoryVersion(), want)
	}
	vs, ok := valueSets[inv.GetInventoryVersion()]
	if !ok {
		return invalidf("inventory_version %d has no value set", inv.GetInventoryVersion())
	}
	if hasUnknown(inv) {
		return invalidf("unknown fields")
	}
	if err := validateHeader(inv, vs); err != nil {
		return err
	}
	if err := validateComponents(inv.GetComponents(), vs); err != nil {
		return err
	}
	if err := validateFeatures(inv.GetFeatures(), vs); err != nil {
		return err
	}
	if err := validateExtensions(inv.GetUnexpectedExtensions(), vs); err != nil {
		return err
	}
	if n := proto.Size(inv); n > int(encodedLimit.Load()) {
		return fmt.Errorf("%w: %w: %d bytes encoded", ErrInvalid, ErrTooLarge, n)
	}
	return nil
}

// hasUnknown reports whether inv or a nested message carries an unknown
// field.
func hasUnknown(inv *providerv1.HostInventory) bool {
	if len(inv.ProtoReflect().GetUnknown()) != 0 {
		return true
	}
	for _, c := range inv.GetComponents() {
		if len(c.ProtoReflect().GetUnknown()) != 0 {
			return true
		}
		for _, f := range c.GetFiles() {
			if len(f.ProtoReflect().GetUnknown()) != 0 {
				return true
			}
		}
	}
	for _, f := range inv.GetFeatures() {
		if len(f.ProtoReflect().GetUnknown()) != 0 {
			return true
		}
	}
	for _, e := range inv.GetUnexpectedExtensions() {
		if len(e.ProtoReflect().GetUnknown()) != 0 {
			return true
		}
	}
	return false
}

func validateHeader(inv *providerv1.HostInventory, vs *valueSet) error {
	st := inv.GetState()
	if st <= providerv1.InventoryState_INVENTORY_STATE_UNSPECIFIED || int32(st) > vs.maxState {
		return invalidf("state %v", st)
	}
	empty := len(inv.GetComponents()) == 0 && len(inv.GetFeatures()) == 0 && len(inv.GetUnexpectedExtensions()) == 0
	tier := inv.GetProcessIdentityTier()
	if int32(tier) < 0 || int32(tier) > vs.maxTier {
		return invalidf("process_identity_tier %v", tier)
	}
	reason := inv.GetStateReason()
	switch st {
	case providerv1.InventoryState_INVENTORY_STATE_COMPLETE:
		if reason != "" || tier == providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED {
			return invalidf("a COMPLETE inventory has a state_reason or no process identity tier")
		}
	case providerv1.InventoryState_INVENTORY_STATE_PENDING:
		if !empty || tier != providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED || inv.GetSnapshotAgeSeconds() != 0 || reason != "" {
			return invalidf("a PENDING inventory lists something, has an age, a tier or a reason")
		}
	case providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE:
		set := vs.overLimit
		if st == providerv1.InventoryState_INVENTORY_STATE_UNREADABLE {
			set = vs.unreadable
		}
		if !empty || tier != providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED || !slices.Contains(set, reason) {
			return invalidf("a %v inventory lists something, has a tier or a state_reason outside its set", st)
		}
	}
	return nil
}

func validateComponents(cs []*providerv1.HostComponent, vs *valueSet) error {
	if len(cs) > MaxComponents {
		return invalidf("%d components", len(cs))
	}
	total := 0
	prev := ""
	for i, c := range cs {
		if i > 0 && c.GetName() <= prev {
			return invalidf("components are not strictly ascending at %q", c.GetName())
		}
		prev = c.GetName()
		if err := validateComponent(c, vs); err != nil {
			return err
		}
		total += len(c.GetFiles())
	}
	if total > MaxFiles {
		return invalidf("%d files", total)
	}
	return nil
}

//nolint:gocyclo // one rule per line of the state-consistency table
func validateComponent(c *providerv1.HostComponent, vs *valueSet) error {
	name := c.GetName()
	if !componentNameRE.MatchString(name) {
		return invalidf("component name %q", name)
	}
	st, origin := c.GetState(), c.GetManifestOrigin()
	if st <= providerv1.ComponentState_COMPONENT_STATE_UNSPECIFIED || int32(st) > vs.maxComponentState {
		return invalidf("component %q state %v", name, st)
	}
	if origin <= providerv1.ManifestOrigin_MANIFEST_ORIGIN_UNSPECIFIED || int32(origin) > vs.maxOrigin {
		return invalidf("component %q manifest_origin %v", name, origin)
	}
	if strings.HasPrefix(name, invalidManifestPrefix) && st != providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID {
		return invalidf("component %q uses the reserved prefix without an invalid manifest", name)
	}
	if err := validateComponentStrings(c); err != nil {
		return err
	}
	reason := c.GetStateReason()
	if st == providerv1.ComponentState_COMPONENT_STATE_READY {
		if reason != "" || origin == providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE || c.GetObservedVersion() == "" {
			return invalidf("READY component %q has a reason, no manifest origin or no observed version", name)
		}
	} else if !slices.Contains(vs.component[st], reason) {
		return invalidf("component %q state_reason %q is not in the set of %v", name, reason, st)
	}
	none := origin == providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE
	switch {
	case st == providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED && !none:
		return invalidf("UNMANIFESTED component %q has a manifest origin", name)
	case none && st != providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED && st != providerv1.ComponentState_COMPONENT_STATE_MISSING:
		return invalidf("component %q has no manifest origin in state %v", name, st)
	case none && (c.GetDeclaredVersion() != "" || c.GetSource() != ""):
		return invalidf("component %q has no manifest origin but a declared version or source", name)
	case !none && c.GetDeclaredVersion() == "" && st != providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID:
		return invalidf("manifested component %q has no declared version", name)
	}
	if (st == providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE) != (c.GetRequiredRange() != "") {
		return invalidf("component %q: required_range is set exactly when INCOMPATIBLE", name)
	}
	if st == providerv1.ComponentState_COMPONENT_STATE_MISSING &&
		(c.GetObservedVersion() != "" || len(c.GetArtifactSha256()) != 0 || len(c.GetFiles()) != 0) {
		return invalidf("MISSING component %q reports a version, digest or file", name)
	}
	if len(c.GetFiles()) > MaxFilesPerComponent {
		return invalidf("component %q has %d files", name, len(c.GetFiles()))
	}
	prev := ""
	for i, f := range c.GetFiles() {
		if i > 0 && f.GetName() <= prev {
			return invalidf("component %q: files are not strictly ascending at %q", name, f.GetName())
		}
		prev = f.GetName()
		if !fileNameRE.MatchString(f.GetName()) || len(f.GetSha256()) != 32 {
			return invalidf("component %q: file %q", name, f.GetName())
		}
	}
	return nil
}

func validateComponentStrings(c *providerv1.HostComponent) error {
	name := c.GetName()
	for field, v := range map[string]string{"observed_version": c.GetObservedVersion(), "declared_version": c.GetDeclaredVersion()} {
		if v != "" && !versionRE.MatchString(v) {
			return invalidf("component %q: %s", name, field)
		}
	}
	if r := c.GetRequiredRange(); r != "" && !rangeRE.MatchString(r) {
		return invalidf("component %q: required_range", name)
	}
	if s := c.GetSource(); s != "" && !sourceRE.MatchString(s) {
		return invalidf("component %q: source", name)
	}
	if n := len(c.GetArtifactSha256()); n != 0 && n != 32 {
		return invalidf("component %q: artifact_sha256 of %d bytes", name, n)
	}
	return nil
}

func validateFeatures(fs []*providerv1.HostFeature, vs *valueSet) error {
	if len(fs) > MaxFeatures {
		return invalidf("%d features", len(fs))
	}
	prev := ""
	for i, f := range fs {
		if i > 0 && f.GetName() <= prev {
			return invalidf("features are not strictly ascending at %q", f.GetName())
		}
		prev = f.GetName()
		if !slices.Contains(vs.features, f.GetName()) {
			return invalidf("feature %q is not in the probe set", f.GetName())
		}
		r := f.GetResult()
		if r <= providerv1.FeatureResult_FEATURE_RESULT_UNSPECIFIED || int32(r) > vs.maxResult {
			return invalidf("feature %q result %v", f.GetName(), r)
		}
		if (r == providerv1.FeatureResult_FEATURE_RESULT_ERROR) != (f.GetReason() != "") {
			return invalidf("feature %q: a reason is set exactly when the result is ERROR", f.GetName())
		}
		if f.GetReason() != "" && !slices.Contains(vs.featReasons, f.GetReason()) {
			return invalidf("feature %q reason %q", f.GetName(), f.GetReason())
		}
	}
	return nil
}

func validateExtensions(es []*providerv1.HostExtension, vs *valueSet) error {
	if len(es) > MaxExtensions {
		return invalidf("%d unexpected extensions", len(es))
	}
	prev := ""
	for i, e := range es {
		if i > 0 && e.GetName() <= prev {
			return invalidf("extensions are not strictly ascending at %q", e.GetName())
		}
		prev = e.GetName()
		if !fileNameRE.MatchString(e.GetName()) {
			return invalidf("extension name %q", e.GetName())
		}
		k := e.GetKind()
		if k <= providerv1.ExtensionKind_EXTENSION_KIND_UNSPECIFIED || int32(k) > vs.maxKind {
			return invalidf("extension %q kind %v", e.GetName(), k)
		}
		n := len(e.GetImageSha256())
		if n != 0 && n != 32 {
			return invalidf("extension %q: image_sha256 of %d bytes", e.GetName(), n)
		}
		if (k == providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY) != (n == 0) {
			return invalidf("extension %q: DIRECTORY exactly when image_sha256 is empty", e.GetName())
		}
	}
	return nil
}

// overLimitOrUnreadable builds the inventory of a snapshot that lists nothing.
func emptyInventory(state providerv1.InventoryState, reason string) *providerv1.HostInventory {
	return &providerv1.HostInventory{State: state, StateReason: reason}
}

// OverLimit returns the inventory that answers a request for version
// requested with OVER_LIMIT and reason, listing nothing: the fallback when a
// report cannot be signed because of its size.
func OverLimit(requested uint32, reason string) *providerv1.HostInventory {
	inv := emptyInventory(providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, reason)
	bind(inv, requested)
	return inv
}

// Unreadable returns the inventory that answers a request for version
// requested with UNREADABLE and reason, listing nothing: what an answer carries
// when the report it had would break a field rule.
func Unreadable(requested uint32, reason string) *providerv1.HostInventory {
	inv := emptyInventory(providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, reason)
	bind(inv, requested)
	return inv
}

// bind sets the version fields of an inventory for a request of requested.
func bind(inv *providerv1.HostInventory, requested uint32) {
	inv.RequestedVersion = requested
	inv.SupportedVersion = SupportedVersion
	inv.InventoryVersion = min(requested, SupportedVersion)
}

// equalDigest reports whether two digests are equal.
func equalDigest(a, b []byte) bool { return bytes.Equal(a, b) }
