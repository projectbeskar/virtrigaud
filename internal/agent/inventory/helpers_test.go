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
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// fakeBoot is a settable CLOCK_BOOTTIME.
type fakeBoot struct {
	mu  sync.Mutex
	now time.Duration
	err error
}

func (b *fakeBoot) BootTime() (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now, b.err
}

func (b *fakeBoot) advance(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now += d
}

// fakeHost is a Host of a healthy modern kernel.
type fakeHost struct {
	ns        map[string]uint64
	cgroup2   bool
	kernel    string
	pidfdErr  error
	handle    bool
	handleErr error
}

func healthyHost() *fakeHost {
	return &fakeHost{
		ns:      map[string]uint64{"pid": initPIDNSInode, "cgroup": initCgroupNSInode, "time": initTimeNSInode},
		cgroup2: true, kernel: "6.15.2-test", handle: true,
	}
}

func (h *fakeHost) NamespaceInode(kind string) (uint64, error) {
	v, ok := h.ns[kind]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return v, nil
}
func (h *fakeHost) Cgroup2Mounted() (bool, error)  { return h.cgroup2, nil }
func (h *fakeHost) KernelRelease() (string, error) { return h.kernel, nil }
func (h *fakeHost) PidfdOpenSelf() error           { return h.pidfdErr }
func (h *fakeHost) PidfsHandle() (bool, error)     { return h.handle, h.handleErr }

// tree is a fixture root directory.
type tree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	return &tree{t: t, root: t.TempDir()}
}

func (tr *tree) path(p string) string { return filepath.Join(tr.root, p) }

func (tr *tree) write(p string, content []byte) {
	tr.t.Helper()
	require.NoError(tr.t, os.MkdirAll(filepath.Dir(tr.path(p)), 0o755))
	require.NoError(tr.t, os.WriteFile(tr.path(p), content, 0o644))
}

func (tr *tree) mkdir(p string) {
	tr.t.Helper()
	require.NoError(tr.t, os.MkdirAll(tr.path(p), 0o755))
}

// manifest writes a manifest file in dir with the given JSON.
func (tr *tree) manifest(dir, base, doc string) {
	tr.t.Helper()
	tr.write(filepath.Join(dir, base), []byte(doc))
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", s[:])
}

func rawDigest(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// Fixture contents.
var (
	chBinary    = []byte("cloud-hypervisor binary")
	agentBinary = []byte("agent binary")
)

const agentExe = "/opt/agent/vrtg-agent"

// manifestJSON is a manifest of component with version and extra fields.
func manifestJSON(component, version, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"schemaVersion":1,"component":%q,"version":%q,"source":"package"%s}`, component, version, extra)
}

// healthyTree is a host with a READY hypervisor and a READY agent component.
func healthyTree(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t)
	tr.write("/usr/bin/cloud-hypervisor", chBinary)
	tr.write(agentExe, agentBinary)
	tr.write("/var/lib/dpkg/status", []byte("Package: bash\nStatus: install ok installed\nVersion: 5.2\n\n"+
		"Package: cloud-hypervisor\nStatus: install ok installed\nVersion: 1.2.3-1\n\n"))
	tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.2.3", ""))
	tr.manifest(ReadOnlyManifestDir, "agent.json", manifestJSON("virtrigaud-agent", "0.1.0", ""))
	return tr
}

// newCollector returns a collector over tr with a healthy host.
func newCollector(t *testing.T, tr *tree, mutate func(*Options)) (*Collector, *fakeBoot) {
	t.Helper()
	boot := &fakeBoot{now: time.Hour}
	l := DefaultLayout()
	l.AgentExecutable = agentExe
	o := Options{
		FS: OSFS{Root: tr.root}, Host: healthyHost(), Boot: boot, Layout: l, AgentVersion: "v0.1.0",
		Log: logr.Discard(), AllowShortTimings: true,
	}
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	require.NoError(t, err)
	return c, boot
}

// collectInventory runs one collection and returns the inventory for version 1.
func collectInventory(t *testing.T, tr *tree, mutate func(*Options)) *providerv1.HostInventory {
	t.Helper()
	c, _ := newCollector(t, tr, mutate)
	c.CollectNow(context.Background())
	inv := c.Inventory(1)
	require.NoError(t, Validate(inv, 1), "what the collector reports always satisfies the field rules")
	return inv
}

// component returns the component called name.
func component(t *testing.T, inv *providerv1.HostInventory, name string) *providerv1.HostComponent {
	t.Helper()
	for _, c := range inv.GetComponents() {
		if c.GetName() == name {
			return c
		}
	}
	t.Fatalf("no component %q in %v", name, inv.GetComponents())
	return nil
}

// countingFS counts the calls that reach the file system.
type countingFS struct {
	FS
	mu    sync.Mutex
	calls int
	dirs  int
}

func (c *countingFS) bump(dir bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if dir {
		c.dirs++
	}
}

func (c *countingFS) ReadDir(p string, limit int) ([]DirEntry, bool, error) {
	c.bump(true)
	return c.FS.ReadDir(p, limit)
}

func (c *countingFS) Lstat(p string) (fs.FileMode, int64, error) {
	c.bump(false)
	return c.FS.Lstat(p)
}

func (c *countingFS) Open(p string, follow bool) (File, error) {
	c.bump(false)
	return c.FS.Open(p, follow)
}

func (c *countingFS) snapshot() (calls, dirs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.dirs
}

var _ = syscall.ENOENT
