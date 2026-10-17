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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provider used to stage XML in fixed, predictable files in the host's
// shared /tmp before `virsh pool-define` / `virsh define` (R9):
//
//   - /tmp/default-pool.xml (createDefaultStoragePool), the same for every
//     caller: two concurrent calls, or two providers sharing a host, overwrote
//     each other's file;
//   - /tmp/<domain>-sync.xml (syncPersistentXML), the same for every sync of
//     one domain, and left behind when the define failed.
//
// A fixed name in a shared /tmp is also a symlink / pre-creation risk. Both now
// hand the XML to virsh on stdin (`/dev/stdin`, as vol-create does), so no file
// is made. These tests pin that: virsh is told to read stdin, receives the whole
// document, and the old fixed paths are never created or touched.

// fixedHostTempFiles are the fixed host paths the provider used to write.
func fixedHostTempFiles(domain string) []string {
	return []string{"/tmp/default-pool.xml", "/tmp/" + domain + "-sync.xml"}
}

// fileStamp records whether path exists and, if so, its size and mtime, so a
// test can prove a shared fixed path was neither created nor modified.
func fileStamp(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "absent"
	}
	require.NoError(t, err)
	return fmt.Sprintf("%s/%d", fi.ModTime(), fi.Size())
}

// stdinCaptures returns the documents the fake virsh in dir received on stdin
// for files named prefix*.xml (one per invocation).
func stdinCaptures(t *testing.T, dir, prefix string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, prefix+"*.xml"))
	require.NoError(t, err)
	var out []string
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // test reads its own fixture
		require.NoError(t, err)
		out = append(out, string(b))
	}
	return out
}

// rawVirshCalls returns every call line (subcommand and arguments) the fake
// pool host logged.
func (f *poolFixture) rawVirshCalls() []string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls.log")) //nolint:gosec // test reads its own fixture log
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(f.t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// TestCreateDefaultStoragePool_DefinesFromStdin: the pool XML reaches
// `virsh pool-define /dev/stdin` whole, and no file is written on the host.
func TestCreateDefaultStoragePool_DefinesFromStdin(t *testing.T) {
	fx := newPoolFixture(t)
	before := fileStamp(t, fixedHostTempFiles("")[0])

	require.NoError(t, fx.storage().createDefaultStoragePool(context.Background()))

	assert.Contains(t, fx.rawVirshCalls(), "pool-define /dev/stdin")
	for _, c := range fx.rawVirshCalls() {
		assert.NotContains(t, c, "default-pool.xml", "no staged file is named to virsh")
	}
	docs := stdinCaptures(t, fx.dir, "pool-define.")
	require.Len(t, docs, 1)
	assert.Contains(t, docs[0], "<name>default</name>")
	assert.Contains(t, docs[0], "<path>"+fx.required+"</path>")
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles("")[0]), "the old fixed path is never created or touched")
}

// TestCreateDefaultStoragePool_DefineFailure: a rejected define is an error
// that leaves nothing behind (there is no file to clean up).
func TestCreateDefaultStoragePool_DefineFailure(t *testing.T) {
	fx := newPoolFixture(t)
	fx.flag("pool.define-fails", "")
	before := fileStamp(t, fixedHostTempFiles("")[0])

	err := fx.storage().createDefaultStoragePool(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to define storage pool")
	assert.Contains(t, err.Error(), "failed to define pool", "virsh's own answer is kept for the log")
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles("")[0]))
	assert.NotContains(t, fx.poolCalls(), "pool-build", "nothing is built after a failed define")
}

// TestCreateDefaultStoragePool_ConcurrentCallsShareNoFile: simultaneous calls
// on one host (two providers, or a clustered fan-out) each hand virsh the whole
// document; with the old fixed /tmp/default-pool.xml they raced on one file.
func TestCreateDefaultStoragePool_ConcurrentCallsShareNoFile(t *testing.T) {
	fx := newPoolFixture(t)
	before := fileStamp(t, fixedHostTempFiles("")[0])

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fx.storage().createDefaultStoragePool(context.Background())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}

	docs := stdinCaptures(t, fx.dir, "pool-define.")
	require.Len(t, docs, callers, "every call defined from its own stdin")
	for _, d := range docs {
		assert.Contains(t, d, "<path>"+fx.required+"</path>")
		assert.True(t, strings.HasSuffix(strings.TrimSpace(d), "</pool>"), "the document is whole: %q", d)
	}
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles("")[0]))
}

// syncDomain is the domain the sync tests act on.
const syncDomain = "vrsync-stdin-web"

// TestSyncPersistentXML_RedefinesFromStdin: the running XML is re-defined from
// stdin; no `/tmp/<domain>-sync.xml` is written, named or removed.
func TestSyncPersistentXML_RedefinesFromStdin(t *testing.T) {
	xml := routingDomainXML(syncDomain, ownerTeamA)
	fx := newOpsFixture(t, map[string]map[string]string{"single": {syncDomain: xml}})
	before := fileStamp(t, fixedHostTempFiles(syncDomain)[1])

	require.NoError(t, syncPersistentXML(context.Background(), localHostVP("single"), syncDomain))

	assert.Equal(t, []string{"single dumpxml " + syncDomain, "single define /dev/stdin"}, fx.calls(),
		"read the running XML, define it from stdin; no host file, so no rm")
	docs := stdinCaptures(t, filepath.Join(fx.dir, "single"), "define-stdin.")
	require.Len(t, docs, 1)
	assert.Equal(t, xml, docs[0], "virsh received the dumped XML unchanged")
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles(syncDomain)[1]), "the old fixed path is never created or touched")
}

// TestSyncPersistentXML_DefineFailure: a failed define is reported and leaves
// no file on the host (it used to leave /tmp/<domain>-sync.xml).
func TestSyncPersistentXML_DefineFailure(t *testing.T) {
	fx := newOpsFixture(t, map[string]map[string]string{"single": {syncDomain: routingDomainXML(syncDomain, ownerTeamA)}})
	fx.script("single", "fail-define", "")
	before := fileStamp(t, fixedHostTempFiles(syncDomain)[1])

	err := syncPersistentXML(context.Background(), localHostVP("single"), syncDomain)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to redefine domain")
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles(syncDomain)[1]))
	for _, c := range fx.calls() {
		assert.False(t, strings.HasPrefix(c, "local "), "no host file command (rm, tee, mktemp) runs: %q", c)
	}
}

// TestSyncPersistentXML_ConcurrentSyncsShareNoFile: simultaneous syncs of one
// domain each define from their own stdin.
func TestSyncPersistentXML_ConcurrentSyncsShareNoFile(t *testing.T) {
	xml := routingDomainXML(syncDomain, ownerTeamA)
	fx := newOpsFixture(t, map[string]map[string]string{"single": {syncDomain: xml}})
	before := fileStamp(t, fixedHostTempFiles(syncDomain)[1])

	const callers = 6
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = syncPersistentXML(context.Background(), localHostVP("single"), syncDomain)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "sync %d", i)
	}

	docs := stdinCaptures(t, filepath.Join(fx.dir, "single"), "define-stdin.")
	require.Len(t, docs, callers)
	for _, d := range docs {
		assert.Equal(t, xml, d)
	}
	assert.Equal(t, before, fileStamp(t, fixedHostTempFiles(syncDomain)[1]))
}

// argvFakeVirsh logs its whole argument vector, one call per line, to
// $FAKE_ARGV_LOG, and answers the two subcommands syncPersistentXML uses.
const argvFakeVirsh = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_ARGV_LOG"
case "$1$3" in
  -cdumpxml) echo "<domain/>" ;;
  -cdefine) cat > /dev/null ;;
esac
`

// TestSyncPersistentXML_ConnectionURI pins the -c URI the persistent-XML sync
// passes. It used to run `virsh define <file>` through the host-shell escape,
// which carries no -c on a non-SSH transport (so the define went to virsh's
// default connection); it now passes the provider's own URI to both the dump and
// the define, as every other call does — including the non-system forms
// (a remote TCP daemon, a ?socket= override), which must arrive whole.
func TestSyncPersistentXML_ConnectionURI(t *testing.T) {
	uris := []string{
		"qemu:///system",
		"qemu:///session",
		"qemu+tcp://kvm-01.example:16509/system",
		"qemu+tls://kvm-01.example/system",
		"qemu:///session?socket=/run/user/1000/libvirt/libvirt-sock",
		"qemu+unix:///system?socket=/var/run/libvirt/libvirt-sock",
	}
	for _, uri := range uris {
		t.Run(uri, func(t *testing.T) {
			bin := t.TempDir()
			logFile := filepath.Join(t.TempDir(), "argv.log")
			require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(argvFakeVirsh), 0o700)) //nolint:gosec // test shim must be executable
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_ARGV_LOG", logFile)

			vp := NewVirshProvider(&ProviderConfig{})
			vp.uri = uri
			require.NoError(t, syncPersistentXML(context.Background(), vp, syncDomain))

			raw, err := os.ReadFile(logFile) //nolint:gosec // test reads its own fixture log
			require.NoError(t, err)
			assert.Equal(t, []string{
				"-c " + uri + " dumpxml " + syncDomain,
				"-c " + uri + " define /dev/stdin",
			}, strings.Split(strings.TrimSpace(string(raw)), "\n"))
		})
	}
}
