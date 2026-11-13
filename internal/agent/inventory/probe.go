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
	"context"
	"errors"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"syscall"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Initial namespace inode numbers: the fixed numbers Linux gives the
// namespaces of the initial (host) set.
const (
	initPIDNSInode    uint64 = 0xEFFFFFFC
	initCgroupNSInode uint64 = 0xEFFFFFFB
	initTimeNSInode   uint64 = 0xEFFFFFFA
)

// Paths the boot-integrity probes read as data.
const (
	// secureBootVariable is the firmware's SecureBoot variable; its content
	// is four attribute bytes and one value byte.
	secureBootVariable = "/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"
	// efiDir exists only on a host booted through EFI firmware.
	efiDir = "/sys/firmware/efi"
	// efivarsDir is where efivarfs is mounted.
	efivarsDir = "/sys/firmware/efi/efivars"
	// securityfsDir is where securityfs is mounted.
	securityfsDir = "/sys/kernel/security"
	// measuredBootLog is the firmware's measured-boot event log.
	measuredBootLog = "/sys/kernel/security/tpm0/binary_bios_measurements"
	// maxVariableBytes bounds the firmware variable read.
	maxVariableBytes = 16
)

// File system magic numbers the boot probes check before they trust what a
// missing file means.
const (
	magicEfivarfs  uint64 = 0xde5e81e4
	magicSecurityf uint64 = 0x73636673
)

// Kernel releases that introduced a feature the agent infers from the kernel
// release (major, minor). The inference cannot see a feature a distribution
// backported, so it errs on the side of ABSENT.
var (
	kernelCloneIntoCgroup = [2]int{5, 7}
	kernelPidfdInfoExit   = [2]int{6, 15}
)

// Host is what the feature probes read of the running kernel. Every method is
// a data read or a system call on the agent itself; none starts a process.
type Host interface {
	// NamespaceInode returns the inode number of the process's namespace of
	// kind ("pid", "cgroup", "time"). An error matching fs.ErrNotExist means
	// the kernel has no such namespace.
	NamespaceInode(kind string) (uint64, error)
	// Cgroup2Mounted reports whether a cgroup v2 hierarchy is mounted at the
	// cgroup root.
	Cgroup2Mounted() (bool, error)
	// KernelRelease returns the kernel release string.
	KernelRelease() (string, error)
	// PidfdOpenSelf opens and closes a process file descriptor of the agent
	// itself; an error matching syscall.ENOSYS means the call is absent.
	PidfdOpenSelf() error
	// PidfsHandle reports whether a file handle taken from a process file
	// descriptor of the agent itself opens the same process again. An
	// absent pidfd_open is false, not an error.
	PidfsHandle() (bool, error)
	// PidfdOnPidfs reports whether the agent's own process file descriptor
	// lives on the process file system (pidfs), which numbers processes
	// uniquely. An absent pidfd_open is false, not an error.
	PidfdOnPidfs() (bool, error)
	// KVMUsable reports whether /dev/kvm is the KVM character device and the
	// agent can open it; false when there is no such device.
	KVMUsable() (bool, error)
	// TunUsable reports whether /dev/net/tun is the TUN character device and
	// the agent can open it; false when there is no such device.
	TunUsable() (bool, error)
	// FSMagic returns the file system type magic of the file system holding
	// path.
	FSMagic(path string) (uint64, error)
}

// probe is one feature probe.
type probe struct {
	name string
	run  func(ctx context.Context, e *env) (providerv1.FeatureResult, string)
}

var (
	present = providerv1.FeatureResult_FEATURE_RESULT_PRESENT
	absent  = providerv1.FeatureResult_FEATURE_RESULT_ABSENT
	errored = providerv1.FeatureResult_FEATURE_RESULT_ERROR
)

// failure maps an error to an ERROR result with a reason of the closed set;
// the error's text is never reported.
func failure(err error) (providerv1.FeatureResult, string) {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return errored, FeatureReasonPermissionDenied
	}
	return errored, FeatureReasonProbeFailed
}

// kernelAtLeast reports whether the kernel release is at least want.
func kernelAtLeast(h Host, want [2]int) (providerv1.FeatureResult, string) {
	rel, err := h.KernelRelease()
	if err != nil {
		return failure(err)
	}
	major, minor, ok := parseKernelRelease(rel)
	if !ok {
		return errored, FeatureReasonProbeFailed
	}
	if major > want[0] || (major == want[0] && minor >= want[1]) {
		return present, ""
	}
	return absent, ""
}

// parseKernelRelease reads "major.minor" from a release such as
// "6.8.0-55-generic".
func parseKernelRelease(rel string) (major, minor int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(rel), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, false
	}
	digits := parts[1]
	for i, r := range digits {
		if r < '0' || r > '9' {
			digits = digits[:i]
			break
		}
	}
	if minor, err = strconv.Atoi(digits); err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// namespaceProbe is a probe that reports PRESENT when the agent runs in the
// initial namespace of kind.
func namespaceProbe(name, kind string, initial uint64) probe {
	return probe{name: name, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
		ino, err := e.host.NamespaceInode(kind)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return absent, ""
		case err != nil:
			return failure(err)
		case ino == initial:
			return present, ""
		}
		return absent, ""
	}}
}

// notEvaluated is the result of a probe the agent cannot decide as a data
// read: ERROR with the reason that says it was not tried.
func notEvaluated(context.Context, *env) (providerv1.FeatureResult, string) {
	return errored, FeatureReasonNotEvaluated
}

// boolProbe maps a yes/no answer or an error to a result.
func boolProbe(ok bool, err error) (providerv1.FeatureResult, string) {
	switch {
	case err != nil:
		return failure(err)
	case ok:
		return present, ""
	}
	return absent, ""
}

// secureBoot reads the firmware's SecureBoot variable. A missing variable
// means "not booted with Secure Boot" only where it can be told: there is no
// EFI firmware at all (ABSENT), or efivarfs is mounted and does not hold it.
// EFI firmware with efivarfs not mounted cannot be told (ERROR).
func secureBoot(_ context.Context, e *env) (providerv1.FeatureResult, string) {
	f, err := e.fs.Open(secureBootVariable, false)
	if err != nil {
		if !isNotExist(err) {
			return failure(err)
		}
		if _, _, derr := e.fs.Lstat(efiDir); derr != nil {
			if isNotExist(derr) {
				return absent, ""
			}
			return failure(derr)
		}
		magic, merr := e.host.FSMagic(efivarsDir)
		if merr != nil {
			return failure(merr)
		}
		if magic != magicEfivarfs {
			return errored, FeatureReasonProbeFailed
		}
		return absent, ""
	}
	defer func() { _ = f.Close() }()
	b, tooLarge, err := readLimited(f, maxVariableBytes)
	if err != nil || tooLarge || len(b) != 5 {
		return errored, FeatureReasonProbeFailed
	}
	if b[4] == 1 {
		return present, ""
	}
	return absent, ""
}

// measuredBoot reports whether a TPM firmware event log is present. It trusts
// a missing log only on a mounted securityfs.
func measuredBoot(_ context.Context, e *env) (providerv1.FeatureResult, string) {
	magic, err := e.host.FSMagic(securityfsDir)
	if err != nil {
		return failure(err)
	}
	if magic != magicSecurityf {
		return errored, FeatureReasonProbeFailed
	}
	_, _, err = e.fs.Lstat(measuredBootLog)
	switch {
	case err == nil:
		return present, ""
	case isNotExist(err):
		return absent, ""
	}
	return failure(err)
}

// defaultProbes returns the feature probes of inventory version 1. Two of
// them cannot be decided as a data read and are reported as ERROR with
// not_evaluated, never by running anything: the hypervisor pause and resume
// check needs a self-test in a sandbox, and the system extension image policy
// is not stored in a file the agent can read.
func defaultProbes() []probe {
	ps := []probe{
		{name: FeatureCgroupV2, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			ok, err := e.host.Cgroup2Mounted()
			switch {
			case err != nil:
				return failure(err)
			case ok:
				return present, ""
			}
			return absent, ""
		}},
		{name: FeatureCloneIntoCgroup, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			return kernelAtLeast(e.host, kernelCloneIntoCgroup)
		}},
		{name: FeaturePidfd, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			err := e.host.PidfdOpenSelf()
			switch {
			case err == nil:
				return present, ""
			case errors.Is(err, syscall.ENOSYS):
				return absent, ""
			}
			return failure(err)
		}},
		{name: FeaturePidfsHandle, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			ok, err := e.host.PidfsHandle()
			switch {
			case err != nil:
				return failure(err)
			case ok:
				return present, ""
			}
			return absent, ""
		}},
		{name: FeaturePidfdInfoExit, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			return kernelAtLeast(e.host, kernelPidfdInfoExit)
		}},
		namespaceProbe(FeatureInitialPIDNamespace, "pid", initPIDNSInode),
		namespaceProbe(FeatureInitialCgroupNamespace, "cgroup", initCgroupNSInode),
		namespaceProbe(FeatureInitialTimeNamespace, "time", initTimeNSInode),
		{name: FeatureHypervisorPauseResume, run: notEvaluated},
		{name: FeatureKVM, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			return boolProbe(e.host.KVMUsable())
		}},
		{name: FeatureTun, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			return boolProbe(e.host.TunUsable())
		}},
		{name: FeatureSecureBoot, run: secureBoot},
		{name: FeatureMeasuredBoot, run: measuredBoot},
		{name: FeatureExtensionImagePolicySigned, run: notEvaluated},
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].name < ps[j].name })
	return ps
}

// runProbes runs every probe once.
func (e *env) runProbes(ctx context.Context) []*providerv1.HostFeature {
	out := make([]*providerv1.HostFeature, 0, len(e.probes))
	for _, p := range e.probes {
		if ctx.Err() != nil {
			return out
		}
		res, reason := p.run(ctx, e)
		out = append(out, &providerv1.HostFeature{Name: p.name, Result: res, Reason: reason})
	}
	return out
}

// tier is the strongest process identity mechanism this host supports, as the
// probes show. The service manager's descriptor store is not used by this
// agent yet, so the strongest tier reported is the persistent file handle.
func (e *env) tier(features []*providerv1.HostFeature) providerv1.ProcessIdentityTier {
	for _, f := range features {
		if f.Name == FeaturePidfsHandle && f.Result == present {
			return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE
		}
	}
	if ok, err := e.host.PidfdOnPidfs(); err == nil && ok {
		return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE
	}
	return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME
}
