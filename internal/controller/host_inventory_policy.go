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

package controller

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Manager flags of the host inventory policy (development builds only: a
// release build refuses to start when any is set).
const (
	// ComponentAllowListFileFlagName names the component allow-list file,
	// read once at start.
	ComponentAllowListFileFlagName = "component-allow-list-file"
	// ComponentAllowListSHA256FlagName pins the allow-list file's SHA-256.
	ComponentAllowListSHA256FlagName = "component-allow-list-sha256"
	// ComponentAllowListSignatureFileFlagName names the allow-list's detached
	// signature file.
	ComponentAllowListSignatureFileFlagName = "component-allow-list-signature-file"
	// ComponentAllowListSignersFileFlagName names the PEM file of the keys
	// one of which must have signed the allow-list. Setting it requires the
	// signature.
	ComponentAllowListSignersFileFlagName = "component-allow-list-signers-file"
	// InsecureSkipComponentAllowListFlagName lets hosts be ready without an
	// allow-list comparison.
	InsecureSkipComponentAllowListFlagName = "insecure-skip-component-allow-list"
	// HostInventoryMaxAgeFlagName overrides the maximum inventory age.
	HostInventoryMaxAgeFlagName = "host-inventory-max-age"
	// HostInventoryRequiredFeaturesFlagName overrides the required feature
	// probes.
	HostInventoryRequiredFeaturesFlagName = "host-inventory-required-features"
	// HostInventoryAcceptedOriginsFlagName overrides the accepted manifest
	// origins.
	HostInventoryAcceptedOriginsFlagName = "host-inventory-accepted-origins"
	// HostInventoryAcceptedTiersFlagName overrides the accepted process
	// identity tiers.
	HostInventoryAcceptedTiersFlagName = "host-inventory-accepted-tiers"
)

// DefaultHostInventoryMaxAge is the default maximum age of a host inventory:
// the agent's collection age plus the time since the manager verified it.
const DefaultHostInventoryMaxAge = 300 * time.Second

// Bounds of the maximum inventory age flag.
const (
	minHostInventoryMaxAge = 60 * time.Second
	maxHostInventoryMaxAge = time.Hour
)

// defaultRequiredFeatures are the feature probes a host must report present
// by default: the enterprise host floor (a unified cgroup v2 hierarchy,
// starting a process into a cgroup, process file descriptors), the agent in
// the initial PID, cgroup and time namespaces, and the KVM and TUN devices.
var defaultRequiredFeatures = []string{
	"cgroup_v2", "clone_into_cgroup", "initial_cgroup_namespace", "initial_pid_namespace", "initial_time_namespace",
	"kvm", "pidfd", "tun",
}

// Names of the manifest origins and process identity tiers in the flags.
var (
	originFlagNames = map[string]providerv1.ManifestOrigin{
		"read-only": providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY,
		"writable":  providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE,
	}
	tierFlagNames = map[string]providerv1.ProcessIdentityTier{
		"pidfd-store":  providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFD_STORE,
		"pidfs-handle": providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
		"pidfs-inode":  providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE,
		"start-time":   providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME,
	}
)

// HostInventoryFlags are the raw values of the host inventory policy flags.
type HostInventoryFlags struct {
	// AllowListFile is --component-allow-list-file.
	AllowListFile string
	// AllowListSHA256 is --component-allow-list-sha256.
	AllowListSHA256 string
	// SignatureFile is --component-allow-list-signature-file.
	SignatureFile string
	// SignersFile is --component-allow-list-signers-file.
	SignersFile string
	// InsecureSkipAllowList is --insecure-skip-component-allow-list.
	InsecureSkipAllowList bool
	// MaxAge is --host-inventory-max-age ("" for the default).
	MaxAge string
	// RequiredFeatures is --host-inventory-required-features ("" for the
	// default).
	RequiredFeatures string
	// AcceptedOrigins is --host-inventory-accepted-origins ("" for the
	// default).
	AcceptedOrigins string
	// AcceptedTiers is --host-inventory-accepted-tiers ("" for the default).
	AcceptedTiers string
}

// isSet reports whether any flag is set.
func (f HostInventoryFlags) isSet() bool {
	return f.AllowListFile != "" || f.AllowListSHA256 != "" || f.SignatureFile != "" || f.SignersFile != "" ||
		f.InsecureSkipAllowList || f.MaxAge != "" || f.RequiredFeatures != "" || f.AcceptedOrigins != "" || f.AcceptedTiers != ""
}

// HostInventoryPolicy is how the manager judges the verified host inventory
// of a host of a Provider whose type runs a per-host agent.
type HostInventoryPolicy struct {
	// AllowList is the component allow-list (nil: none loaded).
	AllowList *hostinventory.AllowList
	// AllowListErr is why a configured allow-list was not loaded (a missing
	// pin or file, a digest that differs from the pin, a signature that does
	// not verify): such a manager serves no Provider whose type runs a
	// per-host agent, and none of their hosts is ready.
	AllowListErr error
	// AllowListSignedBy is the fingerprint of the signer whose signature of
	// the allow-list verified; empty without signers.
	AllowListSignedBy string
	// SkipAllowList is the insecure development-only flag: with no
	// allow-list, components are not compared and do not block.
	SkipAllowList bool
	// AcceptedOrigins are the manifest origins a ready component may have.
	AcceptedOrigins []providerv1.ManifestOrigin
	// AcceptedTiers are the process identity tiers accepted.
	AcceptedTiers []providerv1.ProcessIdentityTier
	// RequiredFeatures are the feature probes that must be present, sorted.
	RequiredFeatures []string
	// MaxAge is the maximum inventory age.
	MaxAge time.Duration
	// overridden names the flags set to other than their default, with
	// their effective values, for the startup log.
	overridden []any
}

// DefaultHostInventoryPolicy is the policy of a manager started without any
// host inventory flag: no allow-list (every host's ComponentsReady is False),
// both manifest origins, every process identity tier, the default required
// features and maximum age.
func DefaultHostInventoryPolicy() *HostInventoryPolicy {
	return &HostInventoryPolicy{
		AcceptedOrigins: []providerv1.ManifestOrigin{providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY,
			providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE},
		AcceptedTiers: []providerv1.ProcessIdentityTier{providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFD_STORE,
			providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE,
			providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME},
		RequiredFeatures: slices.Clone(defaultRequiredFeatures),
		MaxAge:           DefaultHostInventoryMaxAge,
	}
}

// AllowListSHA256 returns the SHA-256 of the loaded allow-list, or "".
func (p *HostInventoryPolicy) AllowListSHA256() string {
	if p == nil || p.AllowList == nil {
		return ""
	}
	return p.AllowList.SHA256()
}

// maxAge returns the maximum inventory age (the default for a nil policy).
func (p *HostInventoryPolicy) maxAge() time.Duration {
	if p == nil || p.MaxAge <= 0 {
		return DefaultHostInventoryMaxAge
	}
	return p.MaxAge
}

// errAllowListNotLoaded wraps every reason a configured allow-list is not in
// force.
var errAllowListNotLoaded = errors.New("the configured component allow-list is not in force")

// fileReader reads the policy's files (os.ReadFile; tests replace it).
var fileReader = os.ReadFile

// ParseHostInventoryPolicy parses the host inventory flags into a policy.
// With no flag set it returns DefaultHostInventoryPolicy in every build. A
// release build refuses any flag. An invalid combination or value is an
// error (the manager does not start); a configured allow-list that cannot be
// put in force (a missing pin or file, a digest that differs, a signature
// that does not verify) is not: it is recorded in AllowListErr, and the
// manager runs without serving any Provider whose type runs a per-host agent.
func ParseHostInventoryPolicy(f HostInventoryFlags) (*HostInventoryPolicy, error) {
	p := DefaultHostInventoryPolicy()
	if !f.isSet() {
		return p, nil
	}
	if !devgate.Compiled {
		return nil, fmt.Errorf("the host inventory flags (--%s and the others) cannot be used with a release build: %w",
			ComponentAllowListFileFlagName, devgate.ErrNotCompiled)
	}
	configured := f.AllowListFile != "" || f.AllowListSHA256 != "" || f.SignersFile != "" || f.SignatureFile != ""
	switch {
	case f.InsecureSkipAllowList && configured:
		return nil, fmt.Errorf("--%s cannot be used together with a component allow-list", InsecureSkipComponentAllowListFlagName)
	case f.SignatureFile != "" && f.SignersFile == "":
		return nil, fmt.Errorf("--%s needs --%s (the keys that may sign the allow-list)", ComponentAllowListSignatureFileFlagName,
			ComponentAllowListSignersFileFlagName)
	}
	p.SkipAllowList = f.InsecureSkipAllowList
	if err := p.parseOverrides(f); err != nil {
		return nil, err
	}
	if configured {
		p.loadAllowList(f)
	}
	return p, nil
}

// parseOverrides parses the flags that override a default.
func (p *HostInventoryPolicy) parseOverrides(f HostInventoryFlags) error {
	if f.MaxAge != "" {
		d, err := time.ParseDuration(f.MaxAge)
		if err != nil || d < minHostInventoryMaxAge || d > maxHostInventoryMaxAge {
			return fmt.Errorf("--%s must be a duration from %s to %s", HostInventoryMaxAgeFlagName, minHostInventoryMaxAge, maxHostInventoryMaxAge)
		}
		p.MaxAge = d
		p.overridden = append(p.overridden, HostInventoryMaxAgeFlagName, d.String())
	}
	if f.RequiredFeatures != "" {
		vs, _ := hostidentity.InventoryValueSetOf(hostidentity.InventoryVersion)
		names, err := parseNameList(f.RequiredFeatures, HostInventoryRequiredFeaturesFlagName, func(n string) bool {
			return n == "none" || slices.Contains(vs.Features, n)
		})
		if err != nil {
			return err
		}
		if slices.Contains(names, "none") {
			if len(names) > 1 {
				return fmt.Errorf("--%s: none cannot be combined with feature names", HostInventoryRequiredFeaturesFlagName)
			}
			names = nil
		}
		p.RequiredFeatures = names
		p.overridden = append(p.overridden, HostInventoryRequiredFeaturesFlagName, strings.Join(names, ","))
	}
	if f.AcceptedOrigins != "" {
		names, err := parseNameList(f.AcceptedOrigins, HostInventoryAcceptedOriginsFlagName, func(n string) bool { _, ok := originFlagNames[n]; return ok })
		if err != nil {
			return err
		}
		p.AcceptedOrigins = nil
		for _, n := range names {
			p.AcceptedOrigins = append(p.AcceptedOrigins, originFlagNames[n])
		}
		p.overridden = append(p.overridden, HostInventoryAcceptedOriginsFlagName, strings.Join(names, ","))
	}
	if f.AcceptedTiers != "" {
		names, err := parseNameList(f.AcceptedTiers, HostInventoryAcceptedTiersFlagName, func(n string) bool { _, ok := tierFlagNames[n]; return ok })
		if err != nil {
			return err
		}
		p.AcceptedTiers = nil
		for _, n := range names {
			p.AcceptedTiers = append(p.AcceptedTiers, tierFlagNames[n])
		}
		p.overridden = append(p.overridden, HostInventoryAcceptedTiersFlagName, strings.Join(names, ","))
	}
	if f.InsecureSkipAllowList {
		p.overridden = append(p.overridden, InsecureSkipComponentAllowListFlagName, "true")
	}
	return nil
}

// parseNameList parses a comma-separated list of names valid for flag,
// sorted and without repeats; an empty list or an unknown name is an error.
func parseNameList(value, flag string, valid func(string) bool) ([]string, error) {
	var out []string
	for _, e := range strings.Split(value, ",") {
		n := strings.TrimSpace(e)
		if n == "" {
			continue
		}
		if !valid(n) {
			return nil, fmt.Errorf("--%s: %q is not a valid name", flag, n)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--%s names nothing", flag)
	}
	slices.Sort(out)
	return out, nil
}

// loadAllowList reads and checks the configured allow-list once. A failure
// is recorded, never fatal.
func (p *HostInventoryPolicy) loadAllowList(f HostInventoryFlags) {
	fail := func(err error) { p.AllowListErr = fmt.Errorf("%w: %w", errAllowListNotLoaded, err) }
	switch {
	case f.AllowListFile == "":
		fail(fmt.Errorf("%w: --%s is not set", hostinventory.ErrDigestMismatch, ComponentAllowListFileFlagName))
		return
	case f.AllowListSHA256 == "":
		fail(fmt.Errorf("%w: --%s is not set", hostinventory.ErrDigestMismatch, ComponentAllowListSHA256FlagName))
		return
	}
	data, err := fileReader(f.AllowListFile)
	if err != nil {
		fail(fmt.Errorf("%w: the allow-list file cannot be read", hostinventory.ErrDigestMismatch))
		return
	}
	opts := hostinventory.LoadOptions{Data: data, PinnedSHA256: f.AllowListSHA256}
	if f.SignersFile != "" {
		pemData, err := fileReader(f.SignersFile)
		if err != nil {
			fail(fmt.Errorf("%w: the signers file cannot be read", hostinventory.ErrUnverified))
			return
		}
		if opts.Signers, err = hostinventory.ParseSigners(pemData); err != nil {
			fail(fmt.Errorf("%w: %w", hostinventory.ErrUnverified, err))
			return
		}
		if f.SignatureFile != "" {
			if opts.Signature, err = fileReader(f.SignatureFile); err != nil {
				fail(fmt.Errorf("%w: the signature file cannot be read", hostinventory.ErrUnverified))
				return
			}
		}
	}
	res, err := hostinventory.Load(opts)
	if err != nil {
		fail(err)
		return
	}
	p.AllowList, p.AllowListSignedBy = res.AllowList, res.SignedBy
}

// allowListNotInForce reports whether an allow-list was configured but is not
// in force.
func (p *HostInventoryPolicy) allowListNotInForce() bool { return p != nil && p.AllowListErr != nil }

// LogStartup logs the policy once at start: every override of a default with
// its effective value, the allow-list's digest and signer, the insecure flag
// as a warning, or why a configured allow-list is not in force.
func (p *HostInventoryPolicy) LogStartup(log logr.Logger) {
	if len(p.overridden) > 0 {
		log.Info("Host inventory policy overrides", p.overridden...)
	}
	switch {
	case p.AllowListErr != nil:
		log.Error(p.AllowListErr, "The component allow-list is not in force: no Provider whose type runs a per-host agent is served and none of their hosts is ready")
	case p.AllowList != nil:
		log.Info("Component allow-list loaded", "sha256", p.AllowList.SHA256(), "signedBy", p.AllowListSignedBy)
	case p.SkipAllowList:
		log.Info("WARNING: --" + InsecureSkipComponentAllowListFlagName + " is set: host components are not compared with an " +
			"allow-list; this manager is not for production use")
	}
}
