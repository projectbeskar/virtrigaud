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

package hostcheck_test

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
)

const (
	testUID       = 1500
	testSignerUID = 1501
	testHostURI   = "spiffe://example.test/virtrigaud/host/ns/host-a"
)

var testCfg = hostcheck.Config{
	StateDir: "/var/lib/virtrigaud-agent",
	PKIDir:   "/var/lib/virtrigaud-agent/pki",
	TrustDir: "/usr/share/virtrigaud/trust",
	UID:      testUID,

	SignerKeyFile:   "/var/lib/virtrigaud-signer/host.key",
	SignerSocketDir: "/run/virtrigaud-signer",
	SignerUID:       testSignerUID,
}

func healthy() *hostchecktest.Fake { return hostchecktest.Healthy(testCfg, testHostURI) }

func run(p hostcheck.Probe) error {
	if err := hostcheck.Run(p, testCfg); err != nil {
		return err
	}
	return hostcheck.CheckHostMarker(p, testCfg.StateDir, testHostURI, testUID)
}

func TestHealthyHostPasses(t *testing.T) {
	if err := run(healthy()); err != nil {
		t.Fatalf("healthy host refused: %v", err)
	}
}

// TestStartRefusals: each start-up check refuses the start with its reason
// on a host that fails it, and only that check is broken each time.
func TestStartRefusals(t *testing.T) {
	trustFile := filepath.Join(testCfg.TrustDir, "host-ca.pem")
	pkiKey := filepath.Join(testCfg.PKIDir, "host.key")
	marker := filepath.Join(testCfg.StateDir, hostcheck.HostMarkerFile)
	for _, tc := range []struct {
		name   string
		edit   func(f *hostchecktest.Fake)
		reason string
	}{
		{"runs as root", func(f *hostchecktest.Fake) { f.UID = 0 }, hostcheck.ReasonRunsAsRoot},
		// Initial namespaces.
		{"pid namespace", func(f *hostchecktest.Fake) { f.Namespaces["pid"] = 4026532000 }, hostcheck.ReasonUnsupportedHost},
		{"cgroup namespace", func(f *hostchecktest.Fake) { f.Namespaces["cgroup"] = 4026532001 }, hostcheck.ReasonUnsupportedHost},
		{"user namespace", func(f *hostchecktest.Fake) { f.Namespaces["user"] = 4026532002 }, hostcheck.ReasonUnsupportedHost},
		{"time namespace", func(f *hostchecktest.Fake) { f.Namespaces["time"] = 4026532003 }, hostcheck.ReasonUnsupportedHost},
		{"pid namespace unreadable", func(f *hostchecktest.Fake) { delete(f.Namespaces, "pid") }, hostcheck.ReasonUnsupportedHost},
		// cgroup v2.
		{"legacy cgroup hierarchy", func(f *hostchecktest.Fake) {
			f.Mounts[1] = hostcheck.Mount{MountPoint: hostcheck.CgroupRoot, FSType: "tmpfs", Source: "tmpfs"}
		}, hostcheck.ReasonUnsupportedHost},
		{"hybrid cgroup hierarchy", func(f *hostchecktest.Fake) {
			f.Mounts[1] = hostcheck.Mount{MountPoint: hostcheck.CgroupRoot + "/unified", FSType: "cgroup2", Source: "cgroup2"}
		}, hostcheck.ReasonUnsupportedHost},
		// State directory owner and mode.
		{"state dir owned by another user", func(f *hostchecktest.Fake) {
			f.Files[testCfg.StateDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o700, UID: 0}
		}, hostcheck.ReasonStateDirInsecure},
		{"state dir readable by group", func(f *hostchecktest.Fake) {
			f.Files[testCfg.StateDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o750, UID: testUID}
		}, hostcheck.ReasonStateDirInsecure},
		{"state dir symlink", func(f *hostchecktest.Fake) {
			f.Files[testCfg.StateDir] = hostcheck.FileInfo{Mode: fs.ModeSymlink | 0o777, UID: testUID}
		}, hostcheck.ReasonStateDirInsecure},
		// Persistent state.
		{"state on tmpfs", func(f *hostchecktest.Fake) {
			f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: "/var/lib", FSType: "tmpfs", Source: "tmpfs"})
		}, hostcheck.ReasonStateNotPersistent},
		{"state on overlay", func(f *hostchecktest.Fake) {
			f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: "/var", FSType: "overlay", Source: "overlay"})
		}, hostcheck.ReasonStateNotPersistent},
		{"state on a network file system", func(f *hostchecktest.Fake) {
			f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: "/var/lib/virtrigaud-agent", FSType: "nfs4", Source: "server:/export"})
		}, hostcheck.ReasonStateNotPersistent},
		{"state not on a block device", func(f *hostchecktest.Fake) {
			f.Mounts[0] = hostcheck.Mount{MountPoint: "/", FSType: "ext4", Source: "rootfs"}
		}, hostcheck.ReasonStateNotPersistent},
		{"state on a loop device", func(f *hostchecktest.Fake) {
			f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: "/var/lib", FSType: "ext4", Source: "/dev/loop3"})
		}, hostcheck.ReasonStateNotPersistent},
		{"provisioning marker missing", func(f *hostchecktest.Fake) {
			delete(f.Files, marker)
			delete(f.Content, marker)
		}, hostcheck.ReasonStateNotPersistent},
		{"provisioning marker names another host", func(f *hostchecktest.Fake) {
			f.Content[marker] = []byte("spiffe://example.test/virtrigaud/host/ns/host-b")
		}, hostcheck.ReasonStateNotPersistent},
		{"provisioning marker writable by others", func(f *hostchecktest.Fake) {
			f.Files[marker] = hostcheck.FileInfo{Mode: 0o666, UID: testUID}
		}, hostcheck.ReasonStateNotPersistent},
		// Key material.
		{"pki dir readable by others", func(f *hostchecktest.Fake) {
			f.Files[testCfg.PKIDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o705, UID: testUID}
		}, hostcheck.ReasonPKIInsecure},
		{"pki dir owned by root", func(f *hostchecktest.Fake) {
			f.Files[testCfg.PKIDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o700, UID: 0}
		}, hostcheck.ReasonPKIInsecure},
		{"key readable by group", func(f *hostchecktest.Fake) {
			f.Files[pkiKey] = hostcheck.FileInfo{Mode: 0o640, UID: testUID}
		}, hostcheck.ReasonPKIInsecure},
		{"key owned by another user", func(f *hostchecktest.Fake) {
			f.Files[pkiKey] = hostcheck.FileInfo{Mode: 0o600, UID: 1501}
		}, hostcheck.ReasonPKIInsecure},
		{"key is a symlink", func(f *hostchecktest.Fake) {
			f.Files[pkiKey] = hostcheck.FileInfo{Mode: fs.ModeSymlink | 0o777, UID: testUID}
		}, hostcheck.ReasonPKIInsecure},
		{"pki dir missing", func(f *hostchecktest.Fake) { delete(f.Files, testCfg.PKIDir) }, hostcheck.ReasonPKIInsecure},
		// The host key belongs to the signer.
		{"agent can read the signer's key file", func(f *hostchecktest.Fake) {
			f.ReadablePaths[testCfg.SignerKeyFile] = true
		}, hostcheck.ReasonHostKeyReadable},
		{"the signer's key file does not exist", func(f *hostchecktest.Fake) {
			delete(f.DeniedPaths, testCfg.SignerKeyFile)
		}, hostcheck.ReasonHostKeyReadable},
		{"a key file in the agent's pki dir, even unreadable", func(f *hostchecktest.Fake) {
			f.Files[filepath.Join(testCfg.PKIDir, "host.key")] = hostcheck.FileInfo{Mode: 0o600, UID: testUID}
		}, hostcheck.ReasonHostKeyReadable},
		{"no key file path configured", func(*hostchecktest.Fake) {}, hostcheck.ReasonHostKeyReadable},
		// The signer's socket directory.
		{"socket dir owned by the agent", func(f *hostchecktest.Fake) {
			f.Files[testCfg.SignerSocketDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o750, UID: testUID}
		}, hostcheck.ReasonSignerSocketInsecure},
		{"socket dir open to others", func(f *hostchecktest.Fake) {
			f.Files[testCfg.SignerSocketDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o755, UID: testSignerUID}
		}, hostcheck.ReasonSignerSocketInsecure},
		{"socket dir writable by its group", func(f *hostchecktest.Fake) {
			f.Files[testCfg.SignerSocketDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o770, UID: testSignerUID}
		}, hostcheck.ReasonSignerSocketInsecure},
		{"socket dir is a symlink", func(f *hostchecktest.Fake) {
			f.Files[testCfg.SignerSocketDir] = hostcheck.FileInfo{Mode: fs.ModeSymlink | 0o777, UID: testSignerUID}
		}, hostcheck.ReasonSignerSocketInsecure},
		{"socket dir missing", func(f *hostchecktest.Fake) { delete(f.Files, testCfg.SignerSocketDir) }, hostcheck.ReasonSignerSocketInsecure},
		// Trust configuration.
		{"trust file writable by the agent", func(f *hostchecktest.Fake) { f.WritablePaths[trustFile] = true }, hostcheck.ReasonTrustConfigInsecure},
		{"trust dir writable by the agent", func(f *hostchecktest.Fake) { f.WritablePaths[testCfg.TrustDir] = true }, hostcheck.ReasonTrustConfigInsecure},
		{"trust file owned by the agent", func(f *hostchecktest.Fake) {
			f.Files[trustFile] = hostcheck.FileInfo{Mode: 0o444, UID: testUID}
		}, hostcheck.ReasonTrustConfigInsecure},
		{"trust file group writable", func(f *hostchecktest.Fake) {
			f.Files[trustFile] = hostcheck.FileInfo{Mode: 0o664}
		}, hostcheck.ReasonTrustConfigInsecure},
		{"trust ancestor world writable", func(f *hostchecktest.Fake) {
			f.Files["/usr/share"] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o777}
		}, hostcheck.ReasonTrustConfigInsecure},
		{"trust ancestor owned by a user", func(f *hostchecktest.Fake) {
			f.Files["/usr/share/virtrigaud"] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o755, UID: 1000}
		}, hostcheck.ReasonTrustConfigInsecure},
		{"trust dir through a symlink", func(f *hostchecktest.Fake) {
			f.Files["/usr/share/virtrigaud"] = hostcheck.FileInfo{Mode: fs.ModeSymlink | 0o777}
		}, hostcheck.ReasonTrustConfigInsecure},
		{"trust file is a symlink", func(f *hostchecktest.Fake) {
			f.Files[trustFile] = hostcheck.FileInfo{Mode: fs.ModeSymlink | 0o777}
		}, hostcheck.ReasonTrustConfigInsecure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := healthy()
			p.Edit(tc.edit)
			cfg := testCfg
			if tc.name == "no key file path configured" {
				cfg.SignerKeyFile = ""
			}
			err := hostcheck.Run(p, cfg)
			if err == nil {
				err = hostcheck.CheckHostMarker(p, cfg.StateDir, testHostURI, testUID)
			}
			f, ok := hostcheck.AsFailure(err)
			if !ok {
				t.Fatalf("err = %v, want a refusal with %s", err, tc.reason)
			}
			if f.Reason != tc.reason {
				t.Fatalf("reason = %s (%v), want %s", f.Reason, f, tc.reason)
			}
		})
	}
}

// TestTrustDirMustBeClean: a relative or unclean trust path is refused.
func TestTrustDirMustBeClean(t *testing.T) {
	for _, dir := range []string{"", "usr/share/virtrigaud/trust", "/usr/share/virtrigaud/../trust", "/usr/share/virtrigaud/trust/"} {
		if err := hostcheck.CheckTrustDir(healthy(), dir); err == nil {
			t.Errorf("trust dir %q accepted", dir)
		}
	}
}

// TestKernelWithoutTimeNamespace: a kernel with no time namespace passes the
// namespace check.
func TestKernelWithoutTimeNamespace(t *testing.T) {
	p := healthy()
	p.Edit(func(f *hostchecktest.Fake) { delete(f.Namespaces, "time") })
	if err := hostcheck.CheckNamespaces(p); err != nil {
		t.Fatal(err)
	}
}

// TestZFSDatasetIsPersistent: a ZFS dataset needs no device path.
func TestZFSDatasetIsPersistent(t *testing.T) {
	p := healthy()
	p.Edit(func(f *hostchecktest.Fake) {
		f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: "/var/lib", FSType: "zfs", Source: "rpool/var/lib"})
	})
	if err := hostcheck.CheckPersistent(p, testCfg.StateDir); err != nil {
		t.Fatal(err)
	}
}
