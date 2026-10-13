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

package libvirt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// poolFakeVirsh is a fake `virsh` for EnsureDefaultStoragePool. It logs every
// call to $FAKE_POOL_HOST/calls.log; pool-dumpxml answers the pool at the
// directory in $FAKE_POOL_HOST/pooldir, or libvirt's "not found" answer when
// pool.missing exists, or an unrelated failure when pool.unreadable exists;
// pool-info reports the state in pool.state (default running).
const poolFakeVirsh = `#!/bin/sh
if [ "$1" = "-c" ]; then shift 2; fi
d="$FAKE_POOL_HOST"
printf '%s\n' "$*" >> "$d/calls.log"
case "$1" in
  pool-dumpxml)
    if [ -f "$d/pool.missing" ]; then
      echo "error: failed to get pool '$2'" >&2
      echo "error: Storage pool not found: no storage pool with matching name '$2'" >&2
      exit 1
    fi
    if [ -f "$d/pool.unreadable" ]; then echo "error: failed to connect to the hypervisor" >&2; exit 1; fi
    printf "<pool type='dir'><name>default</name><target><path>%s</path></target></pool>\n" "$(cat "$d/pooldir")" ;;
  pool-info)
    state=running
    if [ -f "$d/pool.state" ]; then state=$(cat "$d/pool.state"); fi
    printf 'Name:           default\nState:          %s\n' "$state" ;;
  pool-define|pool-build|pool-autostart|pool-start|pool-destroy|pool-undefine) : ;;
  *) echo "fake virsh: unsupported: $*" >&2; exit 1 ;;
esac
`

// poolFixture is a fake libvirt host for EnsureDefaultStoragePool.
type poolFixture struct {
	t        *testing.T
	dir      string
	required string
}

// newPoolFixture installs poolFakeVirsh and roots the required pool directory
// (defaultPoolRequiredDir) in a temporary directory.
func newPoolFixture(t *testing.T) *poolFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shims")
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(poolFakeVirsh), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fx := &poolFixture{t: t, dir: t.TempDir(), required: filepath.Join(t.TempDir(), "images")}
	require.NoError(t, os.MkdirAll(fx.required, 0o750))
	t.Setenv("FAKE_POOL_HOST", fx.dir)
	prev := defaultPoolRequiredDir
	defaultPoolRequiredDir = fx.required
	t.Cleanup(func() { defaultPoolRequiredDir = prev })
	fx.poolAt(fx.required)
	return fx
}

// poolAt places the host's "default" pool at dir.
func (f *poolFixture) poolAt(dir string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, "pooldir"), []byte(dir), 0o600))
}

// flag creates the behaviour file name with content.
func (f *poolFixture) flag(name, content string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600))
}

// storage returns a StorageProvider on the fake host.
func (f *poolFixture) storage() *StorageProvider {
	vp := NewVirshProvider(&ProviderConfig{})
	vp.uri = "test:///kvm-01"
	return NewStorageProvider(vp)
}

// poolCalls returns the virsh subcommands the fake host received.
func (f *poolFixture) poolCalls() []string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(f.t, err)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			out = append(out, fields[0])
		}
	}
	return out
}

// poolMutations are the virsh subcommands that change a storage pool.
var poolMutations = []string{"pool-define", "pool-build", "pool-autostart", "pool-start", "pool-destroy", "pool-undefine"}

// mutationsIn returns the pool-changing subcommands among calls.
func mutationsIn(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, m := range poolMutations {
			if c == m {
				out = append(out, c)
			}
		}
	}
	return out
}

// TestEnsureDefaultStoragePool_NeverRewritesAnExistingPool is the v0.4.0-rc1
// F1 fix: a "default" pool is never destroyed, undefined or redefined. One at
// another directory (an operator's NFS-backed pool) fails closed with an
// actionable InvalidSpec and is left exactly as it is — not even started.
func TestEnsureDefaultStoragePool_NeverRewritesAnExistingPool(t *testing.T) {
	for _, state := range []string{"running", "inactive"} {
		t.Run(state, func(t *testing.T) {
			fx := newPoolFixture(t)
			nfs := filepath.Join(t.TempDir(), "nfs-pool")
			require.NoError(t, os.MkdirAll(nfs, 0o750))
			fx.poolAt(nfs)
			fx.flag("pool.state", state)

			err := fx.storage().EnsureDefaultStoragePool(context.Background())
			require.Error(t, err)
			assert.True(t, contracts.IsInvalidSpec(err), "non-retryable: only the host's administrator can fix it: %v", err)
			msg := err.Error()
			assert.Contains(t, msg, defaultPoolMisplacedMessage)
			assert.NotContains(t, msg, nfs, "the operator's pool path never reaches a status")
			assert.NotContains(t, msg, "kvm-01", "nor does the host")
			assert.Empty(t, mutationsIn(fx.poolCalls()), "the operator's pool is never touched")
		})
	}
}

// TestEnsureDefaultStoragePool_RequiredDir: a pool at the required directory
// is used; it is started only when it is inactive, and nothing else changes.
func TestEnsureDefaultStoragePool_RequiredDir(t *testing.T) {
	for state, want := range map[string][]string{"running": nil, "inactive": {"pool-start"}} {
		t.Run(state, func(t *testing.T) {
			fx := newPoolFixture(t)
			fx.flag("pool.state", state)
			require.NoError(t, fx.storage().EnsureDefaultStoragePool(context.Background()))
			assert.Equal(t, want, mutationsIn(fx.poolCalls()))
		})
	}
}

// TestEnsureDefaultStoragePool_RequiredDirThroughALink: a pool whose path is a
// symbolic link to the required directory (or the reverse) is the required
// directory once both are resolved on the host.
func TestEnsureDefaultStoragePool_RequiredDirThroughALink(t *testing.T) {
	fx := newPoolFixture(t)
	alias := filepath.Join(t.TempDir(), "images-link")
	require.NoError(t, os.Symlink(fx.required, alias))
	fx.poolAt(alias)
	require.NoError(t, fx.storage().EnsureDefaultStoragePool(context.Background()))
	assert.Empty(t, mutationsIn(fx.poolCalls()))
}

// TestEnsureDefaultStoragePool_CreatedOnlyWhenMissing: only a host with no
// "default" pool at all gets one, at the required directory.
func TestEnsureDefaultStoragePool_CreatedOnlyWhenMissing(t *testing.T) {
	fx := newPoolFixture(t)
	fx.flag("pool.missing", "")
	fx.flag("pool.state", "inactive")
	require.NoError(t, fx.storage().EnsureDefaultStoragePool(context.Background()))
	muts := mutationsIn(fx.poolCalls())
	assert.Contains(t, muts, "pool-define")
	assert.NotContains(t, muts, "pool-destroy")
	assert.NotContains(t, muts, "pool-undefine")
}

// TestEnsureDefaultStoragePool_UnreadablePoolChangesNothing: a pool that
// cannot be read (not "not found") is a retryable failure, and nothing is
// created or changed on the strength of it.
func TestEnsureDefaultStoragePool_UnreadablePoolChangesNothing(t *testing.T) {
	fx := newPoolFixture(t)
	fx.flag("pool.unreadable", "")
	err := fx.storage().EnsureDefaultStoragePool(context.Background())
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "%v", err)
	assert.Empty(t, mutationsIn(fx.poolCalls()))
}

// TestCreate_DefaultPoolElsewhereIsInvalidArgument: a single-host Create on
// a host whose "default" pool is elsewhere is answered InvalidArgument with
// the actionable message (the manager backs off on its slow non-retryable
// cadence), and the pool is never touched.
func TestCreate_DefaultPoolElsewhereIsInvalidArgument(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{"single": {}})
	elsewhere := filepath.Join(t.TempDir(), "nfs-pool")
	require.NoError(t, os.MkdirAll(elsewhere, 0o750))
	t.Setenv("FAKE_POOL_DIR", elsewhere)
	p := &Provider{virshProvider: localHostVP("single")}

	_, err := NewServer(p).Create(context.Background(), &providerv1.CreateRequest{Name: "web", ClassJson: `{"CPU":1,"MemoryMiB":512}`})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, codes.InvalidArgument, st.Code(), st.Message())
	assert.Equal(t, defaultPoolMisplacedMessage, st.Message(), "a constant message: no host, no path")
	assert.NotContains(t, st.Message(), elsewhere)
	for _, c := range fx.calls() {
		for _, m := range poolMutations {
			assert.NotContains(t, c, " "+m+" ", "the pool is never touched: %q", c)
		}
	}
}

// TestHostLabel names a remote host by its host name only (never the user or
// port), and a local connection generically.
func TestHostLabel(t *testing.T) {
	assert.Equal(t, `host "kvm-01"`, hostLabel("qemu+ssh://root@kvm-01:2222/system?keyfile=/k"))
	assert.Equal(t, "the libvirt host", hostLabel("qemu:///system"))
	assert.Equal(t, "the libvirt host", hostLabel("test:///kvm-01"))
}
