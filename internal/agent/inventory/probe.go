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
	// measuredBootLog is the firmware's measured-boot event log.
	measuredBootLog = "/sys/kernel/security/tpm0/binary_bios_measurements"
	// maxVariableBytes bounds the firmware variable read.
	maxVariableBytes = 16
)

// Kernel releases that introduced a feature the agent infers from the kernel
// release (major, minor).
var (
	kernelCloneIntoCgroup = [2]int{5, 7}
	kernelPidfdInfoExit   = [2]int{6, 15}
	kernelPidfsInode      = [2]int{6, 9}
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
	// PidfsHandle reports whether file handles of a process file descriptor
	// can be taken.
	PidfsHandle() (bool, error)
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

// defaultProbes returns the feature probes of inventory version 1. Two of
// them cannot be decided as a data read and are reported as ERROR with
// probe_failed, never by running anything: the hypervisor pause and resume
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
		{name: FeatureHypervisorPauseResume, run: func(context.Context, *env) (providerv1.FeatureResult, string) {
			return errored, FeatureReasonProbeFailed
		}},
		{name: FeatureSecureBoot, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			f, err := e.fs.Open(secureBootVariable, false)
			if err != nil {
				if isNotExist(err) {
					return absent, "" // no firmware variable: not booted with Secure Boot
				}
				return failure(err)
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
		}},
		{name: FeatureMeasuredBoot, run: func(_ context.Context, e *env) (providerv1.FeatureResult, string) {
			_, _, err := e.fs.Lstat(measuredBootLog)
			switch {
			case err == nil:
				return present, ""
			case isNotExist(err):
				return absent, ""
			}
			return failure(err)
		}},
		{name: FeatureExtensionImagePolicySigned, run: func(context.Context, *env) (providerv1.FeatureResult, string) {
			return errored, FeatureReasonProbeFailed
		}},
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

// tier is the strongest process identity mechanism the probes show the agent
// can use. The service manager's descriptor store is not used by this agent
// yet, so the strongest tier reported is the persistent file handle.
func (e *env) tier(features []*providerv1.HostFeature) providerv1.ProcessIdentityTier {
	results := map[string]providerv1.FeatureResult{}
	for _, f := range features {
		results[f.Name] = f.Result
	}
	switch {
	case results[FeaturePidfsHandle] == present:
		return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE
	}
	if res, _ := kernelAtLeast(e.host, kernelPidfsInode); res == present && results[FeaturePidfd] == present {
		return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE
	}
	return providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME
}
