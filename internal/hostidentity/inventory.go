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

package hostidentity

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Host inventory in the signed host answer. A GetHostInfo that asks for it
// (GetHostInfoRequest.inventory_version) gets the host's inventory inside the
// signed HostAnswer, so the Provider relaying it cannot alter, replay or
// downgrade it. The manager checks it here independently of the agent's own
// checks: the value sets below are the manager's copy, pinned by test.

// InventoryVersion is the highest host inventory version this manager
// implements: what it asks for.
const InventoryVersion uint32 = 1

// Bounds of a host inventory.
const (
	// MaxInventoryComponents bounds the components.
	MaxInventoryComponents = 32
	// MaxInventoryComponentFiles bounds one component's files.
	MaxInventoryComponentFiles = 8
	// MaxInventoryFiles bounds the files of all components.
	MaxInventoryFiles = 64
	// MaxInventoryFeatures bounds the feature probe results.
	MaxInventoryFeatures = 32
	// MaxInventoryExtensions bounds the unexpected extensions.
	MaxInventoryExtensions = 16
	// MaxInventoryBytes bounds the encoded inventory.
	MaxInventoryBytes = 32 << 10
	// InventoryDigestLen is the length of every digest in an inventory: a
	// raw SHA-256.
	InventoryDigestLen = 32
)

// ErrInventoryEcho is wrapped (with ErrAnswer) when the inventory does not
// echo the inventory version the manager sent, or is present although none
// was asked for. Only a relay that changed the request can cause it, so it is
// "no answer" for this request and nothing else: the next request asks again.
var ErrInventoryEcho = errors.New("the host inventory does not echo the request")

// ErrInventoryInvalid is wrapped (with ErrAnswer) when the inventory breaks a
// field rule although the answer's signature and bindings are good: the
// agent signed malformed content.
var ErrInventoryInvalid = errors.New("the host inventory breaks a field rule")

// InventoryValueSet is the closed value sets of one inventory version: the
// enum values, the state reasons by state, the feature probe names and the
// feature reasons it may carry.
type InventoryValueSet struct {
	// MaxState, MaxComponentState, MaxOrigin, MaxResult, MaxTier and MaxKind
	// are the highest enum values of the version (every value from 1 to the
	// highest is defined).
	MaxState, MaxComponentState, MaxOrigin, MaxResult, MaxTier, MaxKind int32
	// OverLimitReasons are the state reasons of an OVER_LIMIT inventory.
	OverLimitReasons []string
	// UnreadableReasons are the state reasons of an UNREADABLE inventory.
	UnreadableReasons []string
	// ComponentReasons are the state reasons of each component state other
	// than READY.
	ComponentReasons map[providerv1.ComponentState][]string
	// Features are the feature probe names.
	Features []string
	// FeatureReasons are the reasons of an ERROR feature result.
	FeatureReasons []string
}

// inventoryValueSets maps an inventory version to its value sets.
var inventoryValueSets = map[uint32]InventoryValueSet{
	1: {
		MaxState: 4, MaxComponentState: 6, MaxOrigin: 3, MaxResult: 3, MaxTier: 4, MaxKind: 3,
		OverLimitReasons: []string{"too_many_components", "too_many_files", "too_many_features", "too_many_extensions",
			"inventory_too_large"},
		UnreadableReasons: []string{"io_error", "manifest_dir_unreadable", "probe_failed"},
		ComponentReasons: map[providerv1.ComponentState][]string{
			providerv1.ComponentState_COMPONENT_STATE_MISSING:      {"not_installed", "not_declared"},
			providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE: {"version_out_of_range", "version_unreadable"},
			providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY: {"unexpected_extension", "artifact_digest_mismatch",
				"file_digest_mismatch", "socket_unanswered", "self_test_failed", "digest_limit_exceeded", "not_content_addressed"},
			providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED: {"present_undeclared"},
			providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID: {"parse_error", "unsupported_digest",
				"duplicate_declaration", "version_too_long", "invalid_name", "too_large", "unknown_component"},
		},
		Features: []string{"cgroup_v2", "clone_into_cgroup", "pidfd", "pidfs_handle", "pidfd_info_exit",
			"initial_pid_namespace", "initial_cgroup_namespace", "initial_time_namespace", "hypervisor_pause_resume",
			"kvm", "tun", "secure_boot", "measured_boot", "extension_image_policy_signed"},
		FeatureReasons: []string{"probe_failed", "probe_timeout", "permission_denied", "not_evaluated"},
	},
}

// InventoryValueSetOf returns a copy of the value sets of inventory version
// v, and whether the manager implements v.
func InventoryValueSetOf(v uint32) (InventoryValueSet, bool) {
	vs, ok := inventoryValueSets[v]
	if !ok {
		return InventoryValueSet{}, false
	}
	out := vs
	out.OverLimitReasons = slices.Clone(vs.OverLimitReasons)
	out.UnreadableReasons = slices.Clone(vs.UnreadableReasons)
	out.Features = slices.Clone(vs.Features)
	out.FeatureReasons = slices.Clone(vs.FeatureReasons)
	out.ComponentReasons = make(map[providerv1.ComponentState][]string, len(vs.ComponentReasons))
	for k, r := range vs.ComponentReasons {
		out.ComponentReasons[k] = slices.Clone(r)
	}
	return out, true
}

// Field shapes of an inventory.
var (
	invComponentNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	invFileNameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	invVersionRE       = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~_-]{0,63}$`)
	invRangeRE         = regexp.MustCompile(`^[0-9A-Za-z.+~_<>=, -]{1,64}$`)
	invSourceRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// invInvalidManifestPrefix starts the name of a component whose manifest
// cannot be used and names no known component.
const invInvalidManifestPrefix = "manifest-"

// invEcho refuses an answer for an inventory that does not echo the request.
func invEcho(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrAnswer, ErrInventoryEcho, fmt.Sprintf(format, args...))
}

// invInvalid refuses an answer for an inventory that breaks a field rule.
func invInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrAnswer, ErrInventoryInvalid, fmt.Sprintf(format, args...))
}

// VerifyInventory checks the inventory of a verified host answer against the
// inventory version sent in its request (0: none was asked for). A nil
// inventory is never an error: an answer to a request that asked for one but
// carries none is a valid answer without inventory. Otherwise, in this order:
// the inventory is present only when one was asked for and echoes the version
// sent (ErrInventoryEcho); then the agent's supported version is at least 1,
// the inventory version is the lower of the two and one the manager
// implements, and every list is strictly ascending by name and within its
// bounds, the states are consistent, and every enum value, reason and probe
// name is in the version's closed sets (ErrInventoryInvalid). Both errors
// also wrap ErrAnswer: the whole answer is refused.
func VerifyInventory(inv *providerv1.HostInventory, sent uint32) error {
	if inv == nil {
		return nil
	}
	if sent == 0 {
		return invEcho("an inventory the request did not ask for")
	}
	if inv.GetRequestedVersion() != sent {
		return invEcho("requested_version %d is not the %d the request sent", inv.GetRequestedVersion(), sent)
	}
	if inv.GetSupportedVersion() == 0 {
		return invInvalid("supported_version 0")
	}
	if want := min(sent, inv.GetSupportedVersion()); inv.GetInventoryVersion() != want {
		return invInvalid("inventory_version %d is not min(requested, supported) = %d", inv.GetInventoryVersion(), want)
	}
	vs, ok := inventoryValueSets[inv.GetInventoryVersion()]
	if !ok || inv.GetInventoryVersion() > InventoryVersion {
		return invInvalid("inventory_version %d is not implemented", inv.GetInventoryVersion())
	}
	if n := proto.Size(inv); n > MaxInventoryBytes {
		return invInvalid("the inventory encodes to %d bytes", n)
	}
	if err := checkInventoryHeader(inv, &vs); err != nil {
		return err
	}
	if err := checkInventoryComponents(inv.GetComponents(), &vs); err != nil {
		return err
	}
	if err := checkInventoryFeatures(inv.GetFeatures(), &vs); err != nil {
		return err
	}
	return checkInventoryExtensions(inv.GetUnexpectedExtensions(), &vs)
}

// inEnum reports whether v is a defined value from 1 to highest.
func inEnum(v, highest int32) bool { return v >= 1 && v <= highest }

// checkInventoryHeader checks the inventory state rules.
func checkInventoryHeader(inv *providerv1.HostInventory, vs *InventoryValueSet) error {
	st, tier, reason := inv.GetState(), inv.GetProcessIdentityTier(), inv.GetStateReason()
	if !inEnum(int32(st), vs.MaxState) {
		return invInvalid("inventory state %d", st)
	}
	if tier != providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED && !inEnum(int32(tier), vs.MaxTier) {
		return invInvalid("process identity tier %d", tier)
	}
	empty := len(inv.GetComponents()) == 0 && len(inv.GetFeatures()) == 0 && len(inv.GetUnexpectedExtensions()) == 0
	noTier := tier == providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED
	switch st {
	case providerv1.InventoryState_INVENTORY_STATE_COMPLETE:
		if reason != "" || noTier {
			return invInvalid("a complete inventory with a state reason or without a process identity tier")
		}
	case providerv1.InventoryState_INVENTORY_STATE_PENDING:
		if !empty || !noTier || inv.GetSnapshotAgeSeconds() != 0 || reason != "" {
			return invInvalid("a pending inventory that lists something or carries an age, a tier or a reason")
		}
	case providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE:
		set := vs.OverLimitReasons
		if st == providerv1.InventoryState_INVENTORY_STATE_UNREADABLE {
			set = vs.UnreadableReasons
		}
		if !empty || !noTier || !slices.Contains(set, reason) {
			return invInvalid("a %s inventory that lists something, carries a tier or a reason outside its set", st)
		}
	}
	return nil
}

// checkInventoryComponents checks the components, in order.
func checkInventoryComponents(cs []*providerv1.HostComponent, vs *InventoryValueSet) error {
	if len(cs) > MaxInventoryComponents {
		return invInvalid("%d components", len(cs))
	}
	files := 0
	for i, c := range cs {
		if i > 0 && c.GetName() <= cs[i-1].GetName() {
			return invInvalid("the components are not strictly ascending by name")
		}
		if err := checkInventoryComponent(c, vs); err != nil {
			return err
		}
		files += len(c.GetFiles())
	}
	if files > MaxInventoryFiles {
		return invInvalid("%d files", files)
	}
	return nil
}

// checkInventoryComponent checks one component: its strings, its state and
// origin consistency and its files.
//
//nolint:gocyclo // one rule per line of the state-consistency rules
func checkInventoryComponent(c *providerv1.HostComponent, vs *InventoryValueSet) error {
	name, st, origin, reason := c.GetName(), c.GetState(), c.GetManifestOrigin(), c.GetStateReason()
	switch {
	case !invComponentNameRE.MatchString(name):
		return invInvalid("a component name breaks the name rule")
	case !inEnum(int32(st), vs.MaxComponentState):
		return invInvalid("component %q state %d", name, st)
	case !inEnum(int32(origin), vs.MaxOrigin):
		return invInvalid("component %q manifest origin %d", name, origin)
	case strings.HasPrefix(name, invInvalidManifestPrefix) && st != providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID:
		return invInvalid("component %q uses the reserved prefix without an invalid manifest", name)
	case c.GetObservedVersion() != "" && !invVersionRE.MatchString(c.GetObservedVersion()),
		c.GetDeclaredVersion() != "" && !invVersionRE.MatchString(c.GetDeclaredVersion()):
		return invInvalid("component %q: a version breaks the version rule", name)
	case c.GetRequiredRange() != "" && !invRangeRE.MatchString(c.GetRequiredRange()):
		return invInvalid("component %q: the required range breaks its rule", name)
	case c.GetSource() != "" && !invSourceRE.MatchString(c.GetSource()):
		return invInvalid("component %q: the source breaks its rule", name)
	case len(c.GetArtifactSha256()) != 0 && len(c.GetArtifactSha256()) != InventoryDigestLen:
		return invInvalid("component %q: the artifact digest is not %d bytes", name, InventoryDigestLen)
	}
	ready := st == providerv1.ComponentState_COMPONENT_STATE_READY
	none := origin == providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE
	switch {
	case ready && (reason != "" || none || c.GetObservedVersion() == ""):
		return invInvalid("ready component %q with a reason, without a manifest or without an observed version", name)
	case !ready && !slices.Contains(vs.ComponentReasons[st], reason):
		return invInvalid("component %q: the reason is not in the set of %s", name, st)
	case st == providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED && !none:
		return invInvalid("unmanifested component %q with a manifest origin", name)
	case none && st != providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED && st != providerv1.ComponentState_COMPONENT_STATE_MISSING:
		return invInvalid("component %q without a manifest in state %s", name, st)
	case none && (c.GetDeclaredVersion() != "" || c.GetSource() != ""):
		return invInvalid("component %q without a manifest but with a declared version or source", name)
	case !none && c.GetDeclaredVersion() == "" && st != providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID:
		return invInvalid("manifested component %q without a declared version", name)
	case (st == providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE) != (c.GetRequiredRange() != ""):
		return invInvalid("component %q: a required range is set exactly when it is incompatible", name)
	case st == providerv1.ComponentState_COMPONENT_STATE_MISSING &&
		(c.GetObservedVersion() != "" || len(c.GetArtifactSha256()) != 0 || len(c.GetFiles()) != 0):
		return invInvalid("missing component %q reports a version, a digest or files", name)
	case len(c.GetFiles()) > MaxInventoryComponentFiles:
		return invInvalid("component %q has %d files", name, len(c.GetFiles()))
	}
	for i, f := range c.GetFiles() {
		switch {
		case i > 0 && f.GetName() <= c.GetFiles()[i-1].GetName():
			return invInvalid("component %q: the files are not strictly ascending by name", name)
		case !invFileNameRE.MatchString(f.GetName()):
			return invInvalid("component %q: a file name breaks the name rule", name)
		case len(f.GetSha256()) != InventoryDigestLen:
			return invInvalid("component %q: a file digest is not %d bytes", name, InventoryDigestLen)
		}
	}
	return nil
}

// checkInventoryFeatures checks the feature probe results, in order.
func checkInventoryFeatures(fs []*providerv1.HostFeature, vs *InventoryValueSet) error {
	if len(fs) > MaxInventoryFeatures {
		return invInvalid("%d features", len(fs))
	}
	for i, f := range fs {
		r := f.GetResult()
		switch {
		case i > 0 && f.GetName() <= fs[i-1].GetName():
			return invInvalid("the features are not strictly ascending by name")
		case !slices.Contains(vs.Features, f.GetName()):
			return invInvalid("a feature name is not in the probe set")
		case !inEnum(int32(r), vs.MaxResult):
			return invInvalid("feature %q result %d", f.GetName(), r)
		case (r == providerv1.FeatureResult_FEATURE_RESULT_ERROR) != (f.GetReason() != ""):
			return invInvalid("feature %q: a reason is set exactly when the result is an error", f.GetName())
		case f.GetReason() != "" && !slices.Contains(vs.FeatureReasons, f.GetReason()):
			return invInvalid("feature %q: the reason is not in its set", f.GetName())
		}
	}
	return nil
}

// checkInventoryExtensions checks the unexpected extensions, in order.
func checkInventoryExtensions(es []*providerv1.HostExtension, vs *InventoryValueSet) error {
	if len(es) > MaxInventoryExtensions {
		return invInvalid("%d unexpected extensions", len(es))
	}
	for i, e := range es {
		k, n := e.GetKind(), len(e.GetImageSha256())
		switch {
		case i > 0 && e.GetName() <= es[i-1].GetName():
			return invInvalid("the unexpected extensions are not strictly ascending by name")
		case !invFileNameRE.MatchString(e.GetName()):
			return invInvalid("an extension name breaks the name rule")
		case !inEnum(int32(k), vs.MaxKind):
			return invInvalid("extension %q kind %d", e.GetName(), k)
		case n != 0 && n != InventoryDigestLen:
			return invInvalid("extension %q: the image digest is not %d bytes", e.GetName(), InventoryDigestLen)
		case (k == providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY) != (n == 0):
			return invInvalid("extension %q: a directory extension exactly when it has no image digest", e.GetName())
		}
	}
	return nil
}
