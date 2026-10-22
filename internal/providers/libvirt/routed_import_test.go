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
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the provider side of ADR-0007 Addendum B, slice B.1 (R11):
// the routed ImportDisk of a clustered provider. A recording host
// (importRecHost) stands in for the landing host's leased connection: it
// answers every command the import runs, records it, and lets a test fail any
// one of them with a chosen exit status or a dropped session. The routing,
// lease, lock and classification code is the production one (clusterOf's
// registry over host-a and host-b); only the bytes on the host are faked.

// importTarget is the target VirtualMachine of the routed import tests: it
// exists (the migration created it first), so it carries its uid.
var importTarget = contracts.ObjectIdentity{UID: "0b9c3a52-1f4e-4c55-9d1e-7b2f7f0e5a11", Namespace: "team-a", Name: "moved"}

// importTargetWire is importTarget on the wire.
var importTargetWire = &providerv1.ObjectIdentity{Uid: importTarget.UID, Namespace: importTarget.Namespace, Name: importTarget.Name}

const (
	// importDomain and importVolume are the target's domain and landing
	// volume names; importLanded the landing file in the recording host's
	// default pool (importPool).
	importDomain = "team-a.moved"
	importVolume = importDomain + "-migrated"
	importPool   = "/pool"
	importLanded = importPool + "/" + importVolume + ".qcow2"
	importNFSURL = "nfs://nas.example/exports/vmmigrations_team-a_mig_export.qcow2?gid=3000&uid=3000"
)

// importRecCall is one command the recording host ran.
type importRecCall struct {
	// kind is "virsh", "run" or "stream".
	kind string
	// argv is the command as sent (guarded or not).
	argv []string
	// guarded reports that argv runs through hostGuardScript.
	guarded bool
	// timed reports that argv runs under timeout(1) only (the import's
	// cleanup connection: its own removals and the reaper).
	timed bool
	// cmd is the command the guard (and withUmask) wraps: what the host runs.
	cmd []string
}

// key names a call for failure scripting: "virsh <sub>", "qemu-img <sub>",
// "sh head" (the staged-header read), "sh target" / "sh exists" (the guard's
// probes), "stream", or the tool name.
func (c importRecCall) key() string {
	if c.kind == "stream" {
		return "stream"
	}
	cmd := c.cmd
	switch {
	case len(cmd) == 0:
		return ""
	case cmd[0] == "virsh":
		for i := 1; i < len(cmd); i++ {
			if cmd[i] == "-c" {
				i++
				continue
			}
			return "virsh " + cmd[i]
		}
		return "virsh"
	case cmd[0] == "qemu-img" && len(cmd) > 1:
		return "qemu-img " + cmd[1]
	case cmd[0] == "sh" && len(cmd) > 2:
		switch cmd[2] {
		case stagedHeadScript:
			return "sh head"
		case targetKindScript:
			return "sh target"
		case existingPathsScript:
			return "sh exists"
		case reapWriteDirsScript:
			return "sh reap"
		case diskKindScript:
			return "sh kind"
		}
	}
	return cmd[0]
}

// importRecHost is the recording landing host (see the file comment).
type importRecHost struct {
	libvirtConn // the leased connection it stands in for: HostID only

	mu     sync.Mutex
	calls  []importRecCall
	stdin  []byte
	fail   map[string]error  // key -> failure of the first such call
	failN  map[string]int    // key -> which occurrence fails (0: the first)
	seen   map[string]int    // key -> occurrences so far
	doms   map[string]string // landing host domains: name -> dumpxml
	target string            // targetKindScript answer for the landing file
	kind   string            // diskKindScript answer (a landed-disk delete)
	pool   string            // the default pool's directory
	hooks  map[string]func() // key -> run when such a call arrives (outside the lock)
}

func newImportRecHost() *importRecHost {
	return &importRecHost{fail: map[string]error{}, failN: map[string]int{}, seen: map[string]int{}, doms: map[string]string{},
		hooks: map[string]func(){}, pool: importPool, kind: diskKindFile}
}

// wrap is the routedDiskConnFn that hands the import this host instead of the
// leased connection (whose host id it keeps).
func (h *importRecHost) wrap(c libvirtConn) libvirtConn {
	h.libvirtConn = c
	return h
}

func (h *importRecHost) uri() string { return "qemu+ssh://virt@" + string(h.HostID()) + "/system" }

// unguard returns the command the guard (or a bare timeout(1)) wraps, and
// whether argv is guarded, or under timeout(1) only.
func unguard(argv []string) (cmd []string, guarded, timed bool) {
	if len(argv) > 3 && argv[0] == "timeout" {
		return argv[3:], false, true
	}
	if len(argv) < 3 || argv[0] != "sh" || argv[1] != "-c" || argv[2] != hostGuardScript {
		return argv, false, false
	}
	for i := 3; i < len(argv); i++ {
		if argv[i] == "timeout" {
			return argv[i+3:], true, false
		}
	}
	return nil, true, false
}

// unumask strips withUmask's wrapper.
func unumask(argv []string) []string {
	if len(argv) > 5 && argv[0] == "sh" && argv[2] == umaskExecScript {
		return argv[5:]
	}
	return argv
}

// record logs a call and returns its scripted failure, if any.
func (h *importRecHost) record(kind string, argv []string) (importRecCall, error) {
	h.mu.Lock()
	cmd, guarded, timed := unguard(argv)
	c := importRecCall{kind: kind, argv: append([]string(nil), argv...), guarded: guarded, timed: timed, cmd: unumask(cmd)}
	h.calls = append(h.calls, c)
	k := c.key()
	n := h.seen[k]
	h.seen[k] = n + 1
	hook := h.hooks[k]
	var err error
	if e, ok := h.fail[k]; ok && h.failN[k] == n {
		err = e
	}
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c, err
}

func (h *importRecHost) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
	c, err := h.record("virsh", append([]string{"virsh"}, args...))
	if err != nil {
		return failedResult(err), err
	}
	return h.answer(c), nil
}

func (h *importRecHost) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	c, err := h.record("run", argv)
	if err != nil {
		return failedResult(err), err
	}
	return h.answer(c), nil
}

// failedResult is the result a failed command reports: its exit status.
func failedResult(err error) *hostconn.Result {
	code := -1
	if ve, ok := err.(*VirshError); ok {
		code = ve.ExitCode
	}
	return &hostconn.Result{ExitCode: code, Stderr: err.Error()}
}

func (h *importRecHost) StreamIn(_ context.Context, r io.Reader, argv ...string) error {
	_, err := h.record("stream", argv)
	b, rerr := io.ReadAll(r)
	h.mu.Lock()
	h.stdin = b
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return rerr
}

// answer is the host's reply to a command that did not fail.
func (h *importRecHost) answer(c importRecCall) *hostconn.Result {
	cmd := c.cmd
	last := ""
	if len(cmd) > 0 {
		last = cmd[len(cmd)-1]
	}
	out := ""
	switch c.key() {
	case "virsh list":
		if strings.Contains(strings.Join(cmd, " "), "--uuid") {
			return &hostconn.Result{} // the guard's scan: no domain uses anything
		}
		var b strings.Builder
		b.WriteString(" Id   Name                 State\n------------------------------------\n")
		for name := range h.doms {
			fmt.Fprintf(&b, " -    %-20s shut off\n", name)
		}
		out = b.String()
	case "virsh dumpxml":
		out = h.doms[last]
	case "virsh pool-dumpxml":
		out = "<pool type='dir'><name>default</name><target><path>" + h.pool + "</path></target></pool>"
	case "sh kind":
		out = h.kind
	case "sh target":
		if last == h.pool+"/"+importVolume+qcow2Ext {
			out = h.target
		}
	case "realpath":
		var b strings.Builder
		for i, a := range cmd {
			if a == "--" {
				for _, p := range cmd[i+1:] {
					b.WriteString(p + "\x00")
				}
				break
			}
		}
		out = b.String()
	case "mktemp":
		out = strings.TrimSuffix(last, mktempTemplateSuffix) + "0000000000"
	case "stat":
		if len(cmd) > 1 && cmd[1] == "-f" {
			out = "100000000 4096"
		} else {
			out = "1048576"
		}
	case "qemu-img info":
		out = fmt.Sprintf(`{"format": "qcow2", "virtual-size": 1048576, "filename": %q}`, last)
	case "sh head":
		out = base64.StdEncoding.EncodeToString(minimalQCOW2Head())
	}
	return &hostconn.Result{Stdout: out}
}

// failOn makes the n-th (0-based) call of key fail with err.
func (h *importRecHost) failOn(key string, n int, err error) {
	h.fail[key] = err
	h.failN[key] = n
}

// snapshot returns the recorded calls.
func (h *importRecHost) snapshot() []importRecCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]importRecCall(nil), h.calls...)
}

// wrote reports whether any guarded (write-phase) command ran.
func (h *importRecHost) wrote() bool {
	for _, c := range h.snapshot() {
		if c.guarded {
			return true
		}
	}
	return false
}

// importFixture is a clustered provider over host-a and host-b whose routed
// import runs on an importRecHost, and a fake S3 server.
type importFixture struct {
	p   *Provider
	rec *importRecHost
	s3  *s3fake.Server
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	return newImportFixtureOf(t, []string{"host-a", "host-b"}, "host-c")
}

// newImportFixtureOf is newImportFixture over the given live hosts, plus down
// hosts that are in the inventory but can never be dialed.
func newImportFixtureOf(t *testing.T, hosts []string, down ...string) *importFixture {
	t.Helper()
	p := clusterOf(t, hosts, down...)
	p.hostStagingDir = "/var/lib/virtrigaud/staging"
	prev := defaultPoolRequiredDir
	defaultPoolRequiredDir = importPool
	t.Cleanup(func() { defaultPoolRequiredDir = prev })
	rec := newImportRecHost()
	p.routedDiskConnFn = rec.wrap
	srv := s3fake.New("stage")
	t.Cleanup(srv.Close)
	return &importFixture{p: p, rec: rec, s3: srv}
}

func (f *importFixture) server() *Server { return NewServer(f.p) }

// nfsRequest is a routed nfs import of the staged object to host.
func nfsRequest(host string) *providerv1.ImportDiskRequest {
	return &providerv1.ImportDiskRequest{BackendType: migration.BackendNFS, SourceUrl: importNFSURL, Format: "qcow2",
		TargetHostId: host, TargetVm: importTargetWire}
}

// s3Request is a routed s3 import of a staged qcow2 to host.
func (f *importFixture) s3Request(t *testing.T, host string) *providerv1.ImportDiskRequest {
	t.Helper()
	f.s3.Put("exports/disk.qcow2", []byte("staged qcow2 object"))
	js, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendS3, Bucket: "stage",
		Endpoint: f.s3.Endpoint(), UsePathStyle: true})
	require.NoError(t, err)
	return &providerv1.ImportDiskRequest{BackendType: migration.BackendS3, SourceUrl: "s3://stage/exports/disk.qcow2",
		Format: "qcow2", StorageOptionsJson: js, TargetHostId: host, TargetVm: importTargetWire,
		Credentials: map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"}}
}

// callCtx is the manager's 30-minute ImportDisk deadline.
func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// importLockPath is the routed import's host lock for importDomain.
const importLockPath = "/var/lib/virtrigaud/staging/" + hostLockDirName + "/import-" + importDomain + ".lock"

// readPhaseKeys are the only commands a routed import runs on the landing
// host before its write phase: read-only — and the reaper of stale private
// directories (under timeout(1), outside the import's flock).
var readPhaseKeys = map[string]bool{
	"virsh list": true, "virsh dumpxml": true, "virsh pool-dumpxml": true,
	"sh target": true, "sh exists": true, "realpath": true, "sh reap": true,
}

// cleanupSecs is the most timeout(1) a cleanup removal gets: routedCleanupTimeout
// less hostCommandSlack.
var cleanupSecs = int((routedCleanupTimeout - hostCommandSlack) / time.Second)

// timeoutSecsRE matches timeout(1)'s duration argument.
var timeoutSecsRE = regexp.MustCompile(`^([0-9]+)s$`)

// requireGuardedWithinBudget asserts that every call after the first guarded
// one is guarded, with the guard script, flock -n -E 75 on the import's lock
// and timeout(1) ending within the routed budget (the 30-minute call less
// routedBudgetMargin, less hostCommandSlack); and that every call before it
// is a read. It returns the guarded commands, unwrapped.
func requireGuardedWithinBudget(t *testing.T, calls []importRecCall) [][]string {
	t.Helper()
	maxSecs := int((30*time.Minute - routedBudgetMargin - hostCommandSlack) / time.Second)
	var writes [][]string
	writing := false
	timeoutSecs := func(argv []string, i int) int {
		m := timeoutSecsRE.FindStringSubmatch(argv[i])
		require.Len(t, m, 2, "timeout(1)'s duration: %q", argv)
		secs, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		assert.Positive(t, secs, "%q", argv)
		assert.LessOrEqual(t, secs, maxSecs, "timeout(1) ends within the routed budget: %q", argv)
		return secs
	}
	for _, c := range calls {
		if c.timed {
			// The cleanup connection: under timeout(1), never under the
			// import's flock (a still-running earlier command may hold it).
			assert.NotContains(t, c.argv, "flock", "%q", c.argv)
			assert.Equal(t, []string{"timeout", "--kill-after=" + hostCommandKillAfter}, c.argv[:2], "%q", c.argv)
			secs := timeoutSecs(c.argv, 2)
			switch c.key() {
			case "rm":
				require.True(t, writing, "a removal of the import's own files runs in the write phase: %q", c.argv)
				assert.LessOrEqual(t, secs, cleanupSecs, "a removal is bounded by the routed cleanup bound: %q", c.argv)
				writes = append(writes, c.cmd)
			case "sh reap":
				require.False(t, writing, "the reaper runs before the write phase: %q", c.argv)
			default:
				require.Failf(t, "unexpected cleanup command", "%q", c.argv)
			}
			continue
		}
		if !c.guarded {
			require.False(t, writing, "every host command of the write phase runs under the guard: %q", c.argv)
			require.True(t, readPhaseKeys[c.key()], "only reads run before the write phase: %q", c.argv)
			continue
		}
		writing = true
		argv := c.argv
		require.GreaterOrEqual(t, len(argv), 16, "%q", argv)
		assert.Equal(t, []string{"sh", "-c", hostGuardScript, "sh", "/var/lib/virtrigaud/staging/" + hostLockDirName,
			importLockPath, "", "flock", "-n", "-E", strconv.Itoa(flockBusyExit), importLockPath}, argv[:12],
			"guard script, then flock -n on the import's host lock: %q", argv)
		assert.Equal(t, []string{"timeout", "--kill-after=" + hostCommandKillAfter}, argv[12:14], "%q", argv)
		secs := timeoutSecs(argv, 14)
		assert.NotEqual(t, "rm", c.cmd[0], "no removal runs under the import's flock: %q", argv)
		assert.Greater(t, secs, maxSecs-60, "timeout(1) uses the budget it has: %q", argv)
		writes = append(writes, c.cmd)
	}
	require.True(t, writing, "the import wrote")
	return writes
}

// normalizeImportCmd replaces the per-run parts of a command (the staged
// object's unix-time name).
func normalizeImportCmd(cmd []string) string {
	return scdUnixRE.ReplaceAllString(strings.Join(cmd, " "), "<unix>")
}

func TestRoutedImport_NFS_EveryHostCommandGuardedWithinBudget(t *testing.T) {
	f := newImportFixture(t)
	resp, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	require.NoError(t, err)
	assert.Equal(t, importVolume, resp.GetDiskId())
	assert.Equal(t, importLanded, resp.GetPath(), "the single-host landing name, in the landing host's default pool")
	assert.Nil(t, resp.GetTask())

	calls := f.rec.snapshot()
	writes := requireGuardedWithinBudget(t, calls)
	var got []string
	for _, w := range writes {
		got = append(got, normalizeImportCmd(w))
	}
	const wd = importPool + "/" + vmDiskWriteDirPrefix + "0000000000"
	assert.Equal(t, []string{
		"mktemp -d " + importPool + "/" + vmDiskWriteDirPrefix + mktempTemplateSuffix,
		"qemu-img info -U --output=json -f raw " + importNFSURL,
		"stat -f -c %a %S -- " + wd,
		"qemu-img dd -f raw -O raw bs=1M count=2 if=" + importNFSURL + " of=" + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix,
		"stat -c %s -- " + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix,
		"sh -c " + stagedHeadScript + " sh " + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix + " " + strconv.Itoa(migration.StagedImageHeadBytes),
		"qemu-img info --output=json -f qcow2 -- " + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix,
		"qemu-img convert -f qcow2 -O qcow2 " + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix + " " + wd + "/" + importVolume + ".qcow2",
		"rm -f -- " + wd + "/" + importVolume + ".qcow2" + nfsStagedCopySuffix,
		"qemu-img check -f qcow2 " + wd + "/" + importVolume + ".qcow2",
		"mv -f -T -- " + wd + "/" + importVolume + ".qcow2 " + importLanded,
		"virsh -c qemu:///system pool-refresh default",
		"rm -rf -- " + wd,
	}, got, "the single-host nfs landing, every step guarded: copy once, check, convert, check, one rename")

	// The read phase: the name check, the pool, the landing file probes, then
	// the reaper of stale private directories.
	var reads []string
	for _, c := range calls {
		if !c.guarded && c.key() != "rm" {
			reads = append(reads, c.key())
		}
	}
	assert.Equal(t, []string{"virsh list", "virsh pool-dumpxml", "sh target", "sh exists", "sh reap"}, reads,
		"no domain of the name, nothing at the landing names: no other host is scanned")
}

func TestRoutedImport_S3_EveryHostCommandGuardedWithinBudget(t *testing.T) {
	f := newImportFixture(t)
	resp, err := f.server().ImportDisk(callCtx(t), f.s3Request(t, "host-b"))
	require.NoError(t, err)
	assert.Equal(t, importLanded, resp.GetPath())
	assert.EqualValues(t, len("staged qcow2 object"), resp.GetActualSizeBytes())
	assert.Equal(t, "staged qcow2 object", string(f.rec.stdin), "the pod streamed the object into the host's private directory")

	writes := requireGuardedWithinBudget(t, f.rec.snapshot())
	var got []string
	for _, w := range writes {
		got = append(got, normalizeImportCmd(w))
	}
	const wd = importPool + "/" + vmDiskWriteDirPrefix + "0000000000"
	stage := wd + "/.virtrigaud-import-" + importVolume + "-<unix>.qcow2"
	assert.Equal(t, []string{
		"mktemp -d " + importPool + "/" + vmDiskWriteDirPrefix + mktempTemplateSuffix,
		"stat -f -c %a %S -- " + wd,
		"sh -c " + writeStdinToFileScript + " sh " + stage,
		"sh -c " + stagedHeadScript + " sh " + stage + " " + strconv.Itoa(migration.StagedImageHeadBytes),
		"qemu-img info --output=json -f qcow2 -- " + stage,
		"qemu-img convert -f qcow2 -O qcow2 " + stage + " " + wd + "/" + importVolume + ".qcow2",
		"qemu-img check " + wd + "/" + importVolume + ".qcow2",
		"mv -f -T -- " + wd + "/" + importVolume + ".qcow2 " + importLanded,
		"virsh -c qemu:///system pool-refresh default",
		"rm -rf -- " + wd,
	}, got, "the stdin writer runs under the guard too")
}

// TestRoutedImport_RefusedBeforeAnyHost: every refusal of the request itself
// is a constant InvalidArgument, before any host is leased or any command run.
func TestRoutedImport_RefusedBeforeAnyHost(t *testing.T) {
	f := newImportFixture(t)
	for name, mutate := range map[string]func(r *providerv1.ImportDiskRequest){
		"pvc (read in the pod)": func(r *providerv1.ImportDiskRequest) { r.BackendType = migration.BackendPVC },
		"legacy empty backend":  func(r *providerv1.ImportDiskRequest) { r.BackendType = "" },
		"unknown backend":       func(r *providerv1.ImportDiskRequest) { r.BackendType = "ftp" },
		"no target host":        func(r *providerv1.ImportDiskRequest) { r.TargetHostId = " " },
		"no target VM":          func(r *providerv1.ImportDiskRequest) { r.TargetVm = nil },
		"no target VM uid": func(r *providerv1.ImportDiskRequest) {
			r.TargetVm = &providerv1.ObjectIdentity{Namespace: "team-a", Name: "moved"}
		},
		"a uid that is no UUID":     func(r *providerv1.ImportDiskRequest) { r.TargetVm.Uid = "../../etc" },
		"another storage pool":      func(r *providerv1.ImportDiskRequest) { r.StorageHint = "fast" },
		"nfs from a non-nfs source": func(r *providerv1.ImportDiskRequest) { r.SourceUrl = "file:///etc/shadow" },
		"a target name for another VM": func(r *providerv1.ImportDiskRequest) {
			r.TargetName = "other-migrated"
		},
		"a namespace that cannot name a domain": func(r *providerv1.ImportDiskRequest) {
			r.TargetVm = &providerv1.ObjectIdentity{Uid: importTarget.UID, Namespace: "Team_A", Name: "moved"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := nfsRequest("host-b")
			req.TargetVm = &providerv1.ObjectIdentity{Uid: importTarget.UID, Namespace: "team-a", Name: "moved"}
			mutate(req)
			_, err := f.server().ImportDisk(callCtx(t), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		})
	}
	t.Run("s3 in direct mode", func(t *testing.T) {
		req := f.s3Request(t, "host-b")
		req.TransferMode = migration.TransferModeDirect
		_, err := f.server().ImportDisk(callCtx(t), req)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
	t.Run("s3 without credentials", func(t *testing.T) {
		req := f.s3Request(t, "host-b")
		req.Credentials = nil
		_, err := f.server().ImportDisk(callCtx(t), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assertConstantText(t, err, "AK", "SK", "stage")
	})
	assert.Empty(t, f.rec.snapshot(), "a refused import never reaches a host")
	assert.Equal(t, wireRoutedImportBackend, status.Convert(func() error {
		_, err := f.server().ImportDisk(callCtx(t), &providerv1.ImportDiskRequest{BackendType: "pvc", TargetHostId: "host-b", TargetVm: importTargetWire})
		return err
	}()).Message(), "the pvc refusal is a constant text")
}

// TestRoutedImport_PoolElsewhereIsRefused: a landing host whose default pool
// is not where Create attaches an imported disk is refused before anything is
// written: FailedPrecondition scoped to the host (HOST_STORAGE_MISCONFIGURED),
// so the manager can place the target elsewhere (#415).
func TestRoutedImport_PoolElsewhereIsRefused(t *testing.T) {
	f := newImportFixture(t)
	defaultPoolRequiredDir = "/var/lib/libvirt/images"
	_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	st := status.Convert(err)
	assert.Equal(t, codes.FailedPrecondition, st.Code(), "%v", err)
	reasons := errorInfoReasons(st)
	assert.True(t, reasons[contracts.HostStorageMisconfiguredReason], "%v", st.Details())
	assert.True(t, reasons[contracts.VMOperationFailedReason], "out of the breaker")
	assert.True(t, contracts.IsHostStorageMisconfigured(err))
	assert.Equal(t, fmt.Sprintf(wireRoutedImportPoolMisplacedFmt, "host-b"), st.Message())
	assert.NotContains(t, st.Message(), importPool, "the pool's path is never named")
	assert.False(t, f.rec.wrote())
}

// TestRoutedImport_StagedFormatIsChecked: a staged format the import cannot
// read is InvalidArgument before any host is leased (#415).
func TestRoutedImport_StagedFormatIsChecked(t *testing.T) {
	f := newImportFixture(t)
	for _, tc := range []struct {
		s3     bool
		format string
		ok     bool
	}{
		{s3: true, format: "", ok: true}, {s3: true, format: "QCOW2", ok: true}, {s3: true, format: "raw", ok: true},
		{s3: true, format: "vmdk", ok: true}, {s3: true, format: "vhdx"}, {s3: true, format: "qcow2 -o x"},
		{format: "", ok: true}, {format: "qcow2", ok: true}, {format: "raw"}, {format: "vmdk"},
	} {
		req := nfsRequest("host-b")
		if tc.s3 {
			req = f.s3Request(t, "host-b")
		}
		req.Format = tc.format
		err := parseErr(req)
		if tc.ok {
			assert.NoError(t, err, "s3=%v %q", tc.s3, tc.format)
			continue
		}
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "s3=%v %q", tc.s3, tc.format)
		assert.Equal(t, wireRoutedImportFormat, status.Convert(err).Message())
	}
	_, err := f.server().ImportDisk(callCtx(t), func() *providerv1.ImportDiskRequest {
		r := nfsRequest("host-b")
		r.Format = "vmdk"
		return r
	}())
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, f.rec.snapshot(), "refused before any host")
}

// parseErr is parseRoutedImport's error.
func parseErr(req *providerv1.ImportDiskRequest) error {
	_, err := parseRoutedImport(req)
	return err
}

// TestRoutedImport_TooLittleBudgetIsNotStarted: when the checks leave less
// than routedImportMinWriteBudget, the write phase is not started and the
// answer is "not performed", never an unknown outcome (#415).
func TestRoutedImport_TooLittleBudgetIsNotStarted(t *testing.T) {
	f := newImportFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), routedBudgetMargin+30*time.Second)
	defer cancel()
	_, err := f.server().ImportDisk(ctx, nfsRequest("host-b"))
	assertNotPerformedStatus(t, err)
	assert.Equal(t, wireRoutedImportBudgetShort, status.Convert(err).Message())
	assert.False(t, f.rec.wrote(), "nothing was written")
}

// TestRoutedImport_ReaperAndCleanupNeverTakeTheImportLock: the reaper of
// stale private directories runs before the write phase and every removal of
// the import's own files runs under timeout(1) only — never under the
// import's flock, which a still-running command of an earlier call may hold
// (#415: the directory was leaked with exit 75). A removal that fails is
// logged and changes no answer.
func TestRoutedImport_ReaperAndCleanupNeverTakeTheImportLock(t *testing.T) {
	f := newImportFixture(t)
	f.rec.failOn("qemu-img convert", 0, virshFailure(1, "x"))
	f.rec.failOn("rm", 0, virshFailure(flockBusyExit, "x"))
	_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	assertNotPerformedStatus(t, err)
	assert.NotContains(t, status.Convert(err).Message(), "still running", "a removal's status is not the import's")

	var reaper, removal *importRecCall
	for _, c := range f.rec.snapshot() {
		switch c.key() {
		case "sh reap":
			reaper = &c
		case "rm":
			removal = &c
		}
	}
	require.NotNil(t, reaper)
	require.NotNil(t, removal)
	assert.Equal(t, []string{"sh", "-c", reapWriteDirsScript, "sh", importPool, vmDiskWriteDirPrefix,
		strconv.Itoa(int(routedImportReapAge / time.Minute))}, reaper.cmd)
	assert.Equal(t, 65, int(routedImportReapAge/time.Minute), "the longest budget plus the 35-minute settle time")
	for _, c := range []*importRecCall{reaper, removal} {
		assert.True(t, c.timed, "%q", c.argv)
		assert.NotContains(t, c.argv, "flock", "%q", c.argv)
	}
	assert.Equal(t, []string{"rm", "-rf", "--", importPool + "/" + vmDiskWriteDirPrefix + "0000000000"}, removal.cmd)
}

// TestReapWriteDirsScript runs the reaper's script for real: only directories
// with the prefix, directly in the pool, owned by the user and older than the
// age are removed — never a symbolic link (or what it points at), a file, a
// younger directory, or anything deeper.
func TestReapWriteDirsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	pool, outside := t.TempDir(), t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, aged bool) string {
		p := filepath.Join(pool, name)
		require.NoError(t, os.MkdirAll(filepath.Join(p, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(p, "sub", "disk"), []byte("x"), 0o600))
		if aged {
			require.NoError(t, os.Chtimes(p, old, old))
		}
		return p
	}
	stale := mk(vmDiskWriteDirPrefix+"aaaaaaaaaa", true)
	young := mk(vmDiskWriteDirPrefix+"bbbbbbbbbb", false)
	other := mk("team-a.web-disk", true)
	target := filepath.Join(outside, "victim")
	require.NoError(t, os.MkdirAll(target, 0o700))
	link := filepath.Join(pool, vmDiskWriteDirPrefix+"cccccccccc")
	require.NoError(t, os.Symlink(target, link))
	file := filepath.Join(pool, vmDiskWriteDirPrefix+"dddddddddd")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(file, old, old))

	out, err := exec.Command("sh", "-c", reapWriteDirsScript, "sh", pool, vmDiskWriteDirPrefix, "65").CombinedOutput() //nolint:gosec // fixed test script
	require.NoError(t, err, "%s", out)
	assert.NoDirExists(t, stale, "a stale private directory is reaped")
	assert.DirExists(t, young, "a directory a call may still use is kept")
	assert.DirExists(t, other)
	assert.DirExists(t, target, "a link is never followed")
	_, lerr := os.Lstat(link)
	assert.NoError(t, lerr, "a link is not a directory: kept")
	assert.FileExists(t, file)
}

// TestRoutedImport_CallCancelledMidConvert: the manager gives up while the
// convert runs (the call's context is cancelled). The outcome is unknown —
// the convert may still run on the host — and the private directory is still
// removed (the cleanup survives the cancellation).
func TestRoutedImport_CallCancelledMidConvert(t *testing.T) {
	f := newImportFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	f.rec.hooks["qemu-img convert"] = cancel
	f.rec.failOn("qemu-img convert", 0, context.Canceled)
	_, err := f.server().ImportDisk(ctx, nfsRequest("host-b"))
	assertUnknownOutcome(t, err)
	calls := f.rec.snapshot()
	last := calls[len(calls)-1]
	assert.Equal(t, "rm", last.key(), "the private directory is removed after the cancellation")
	assert.True(t, last.timed)
}

// TestRoutedImport_NoStagedLocationAtWarn: the routed import's WARN lines
// never carry the staged object's nfs:// URL (#415).
func TestRoutedImport_NoStagedLocationAtWarn(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })

	f := newImportFixture(t)
	f.rec.failOn("qemu-img dd", 0, &VirshError{Command: "qemu-img dd if=" + importNFSURL, ExitCode: 1,
		Stderr: "qemu-img: Could not open '" + importNFSURL + "'", Cause: &exec.ExitError{}})
	_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	assertNotPerformedStatus(t, err)
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARN") || strings.Contains(l, "not performed") {
			assert.NotContains(t, l, "nfs://", "a staged location in a WARN/ERROR outcome line: %s", l)
		}
	}
	assert.Contains(t, out, stagedLocationMarker, "the cause is still logged, with the location replaced")
}

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestRoutedImport_HostLeaseFailureIsHostUnavailable: an unknown host, or one
// whose dial fails, is HOST_UNAVAILABLE before anything runs.
func TestRoutedImport_HostLeaseFailureIsHostUnavailable(t *testing.T) {
	for _, host := range []string{"host-x", "host-c"} {
		t.Run(host, func(t *testing.T) {
			f := newImportFixture(t)
			_, err := f.server().ImportDisk(callCtx(t), nfsRequest(host))
			st := status.Convert(err)
			assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
			assert.True(t, hasHostUnavailableInfo(st), "%v", err)
			assert.Empty(t, f.rec.snapshot())
		})
	}
}

// TestRoutedImport_LandingHostNameCheck: a domain named after the target VM on
// the landing host refuses the import before any file is looked at (B2 step 1).
func TestRoutedImport_LandingHostNameCheck(t *testing.T) {
	stamped := func(owner contracts.ObjectIdentity) string {
		return guardDomainXML(importDomain, uuidVMOnB, "/pool/x.qcow2", owner, false)
	}
	for name, tc := range map[string]struct {
		xml      string
		reason   string
		own      bool
		contains string
	}{
		"stamped with the target VM's own uid: already created, never imported again": {
			xml: stamped(importTarget), reason: contracts.VMPreviousIncarnationReason, own: true, contains: "already exists on the landing host",
		},
		"a previous incarnation (same namespace and name, another uid)": {
			xml:    stamped(contracts.ObjectIdentity{UID: "5e1f0c3a-9b8d-4e7f-a6c5-4b3a2d1e0f9a", Namespace: "team-a", Name: "moved"}),
			reason: contracts.VMPreviousIncarnationReason,
		},
		"a foreign domain": {xml: stamped(ownerForeign), contains: wireRoutedImportNameTaken},
		"an unstamped domain": {
			xml: guardDomainXML(importDomain, uuidVMOnB, "/pool/x.qcow2", contracts.ObjectIdentity{}, false), contains: wireRoutedImportNameTaken,
		},
		"an unreadable stamp": {xml: "<domain><name>" + importDomain + "</name>", contains: wireRoutedImportNameTaken},
	} {
		t.Run(name, func(t *testing.T) {
			f := newImportFixture(t)
			f.rec.doms[importDomain] = tc.xml
			_, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
			st := status.Convert(err)
			assert.Equal(t, codes.AlreadyExists, st.Code(), "%v", err)
			reasons := errorInfoReasons(st)
			if tc.reason != "" {
				assert.True(t, reasons[tc.reason], "%v", st.Details())
			} else {
				assert.Empty(t, reasons, "a plain AlreadyExists: the manager releases its target's pending host")
			}
			assert.Equal(t, tc.own, ownKind(st), "the own kind is marked for the manager")
			if tc.contains != "" {
				assert.Contains(t, st.Message(), tc.contains)
			}
			assert.NotContains(t, st.Message(), "team-z", "the other owner is never named")
			assert.False(t, f.rec.wrote(), "nothing is landed")
			for _, c := range f.rec.snapshot() {
				assert.Contains(t, []string{"virsh list", "virsh dumpxml"}, c.key(), "only the name check ran")
			}
		})
	}
}

// ownKind reports whether st carries the VM_PREVIOUS_INCARNATION own kind.
func ownKind(st *status.Status) bool {
	for _, d := range st.Details() {
		if info, ok := d.(interface{ GetMetadata() map[string]string }); ok &&
			info.GetMetadata()[contracts.VMPreviousIncarnationKindKey] == contracts.VMPreviousIncarnationKindOwn {
			return true
		}
	}
	return false
}

// assertInProgress asserts the "same import still running" answer: retryable,
// VM_OPERATION_FAILED, naming the host.
func assertInProgress(t *testing.T, err error) {
	t.Helper()
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason), "%v", err)
	assert.Contains(t, st.Message(), "still running")
}

// TestRoutedImport_Outcomes: every failure point of the routed import, and the
// answer the manager reads (ADR-0007 Addendum B, B4; docs/provider-call-outcomes.md).
func TestRoutedImport_Outcomes(t *testing.T) {
	ended := func(code int) error { return virshFailure(code, "scripted") }
	dropped := virshFailure(-1, "ssh: connection reset")
	for name, tc := range map[string]struct {
		key    string
		n      int
		err    error
		s3     bool
		assert func(t *testing.T, err error)
	}{
		// The read phase.
		"the first read finds the host down: HOST_UNAVAILABLE (the host never answered)": {
			key: "virsh list", err: dropped,
			assert: func(t *testing.T, err error) {
				st := status.Convert(err)
				assert.Equal(t, codes.Unavailable, st.Code())
				assert.True(t, hasHostUnavailableInfo(st), "%v", err)
			},
		},
		"the host drops after it answered a read: not performed, never HOST_UNAVAILABLE": {
			key: "virsh pool-dumpxml", err: dropped,
			assert: func(t *testing.T, err error) {
				assertNotPerformedStatus(t, err)
				assert.False(t, hasHostUnavailableInfo(status.Convert(err)))
			},
		},
		"a read fails on the host: not performed": {key: "sh target", err: ended(1), assert: assertNotPerformedStatus},
		// The write phase: commands that ended with an exit status of their own.
		"the private directory cannot be made: not performed":       {key: "mktemp", err: ended(1), assert: assertNotPerformedStatus},
		"the bounded copy fails: not performed":                     {key: "qemu-img dd", err: ended(1), assert: assertNotPerformedStatus},
		"the convert fails: not performed":                          {key: "qemu-img convert", err: ended(1), assert: assertNotPerformedStatus},
		"the converted disk does not check: not performed":          {key: "qemu-img check", err: ended(3), assert: assertNotPerformedStatus},
		"the cleanup's removal exits 75: still not performed":       {key: "qemu-img convert", err: ended(1), assert: assertNotPerformedStatus},
		"the rename fails on its own: failed, nothing landed":       {key: "mv", err: ended(1), assert: assertNotPerformedStatus},
		"the s3 stdin writer fails on its own: not performed":       {key: "stream", err: ended(1), s3: true, assert: assertNotPerformedStatus},
		"the cleanup's session drops: still not performed":          {key: "qemu-img convert", err: ended(1), assert: assertNotPerformedStatus},
		"a pool-refresh failure after the rename is harmless":       {key: "virsh pool-refresh", err: ended(1), assert: func(t *testing.T, err error) { assert.NoError(t, err) }},
		"the staged header is refused: InvalidArgument":             {key: "qemu-img info", n: 1, err: virshFailure(qemuImgFailureExitCode, "bad"), assert: func(t *testing.T, err error) { assertRefusalStatus(t, err, codes.InvalidArgument) }},
		"the same import still runs on the host (flock busy)":       {key: "mktemp", err: ended(flockBusyExit), assert: assertInProgress},
		"the host guard refuses the lock directory":                 {key: "qemu-img convert", err: ended(hostGuardRefusedExit), assert: func(t *testing.T, err error) { assertRefusalStatus(t, err, codes.FailedPrecondition) }},
		"a host tool is missing (flock, timeout, qemu-img)":         {key: "mktemp", err: ended(commandNotFound), assert: func(t *testing.T, err error) { assertRefusalStatus(t, err, codes.FailedPrecondition) }},
		"the convert is stopped by timeout(1): unknown":             {key: "qemu-img convert", err: ended(timeoutExpiredExit), assert: assertUnknownOutcome},
		"the convert is killed: unknown":                            {key: "qemu-img convert", err: ended(timeoutKilledExit), assert: assertUnknownOutcome},
		"the convert's session drops: unknown (it may still run)":   {key: "qemu-img convert", err: dropped, assert: assertUnknownOutcome},
		"the rename's session drops: unknown (it may have renamed)": {key: "mv", err: dropped, assert: assertUnknownOutcome},
		"the s3 stdin writer's session drops: unknown":              {key: "stream", err: dropped, s3: true, assert: assertUnknownOutcome},
	} {
		t.Run(name, func(t *testing.T) {
			f := newImportFixture(t)
			f.rec.failOn(tc.key, tc.n, tc.err)
			switch {
			case strings.HasPrefix(name, "the cleanup's session drops"):
				f.rec.failOn("rm", 0, dropped) // the private directory's removal
			case strings.HasPrefix(name, "the cleanup's removal exits 75"):
				f.rec.failOn("rm", 0, ended(flockBusyExit))
			}
			req := nfsRequest("host-b")
			if tc.s3 {
				req = f.s3Request(t, "host-b")
			}
			_, err := f.server().ImportDisk(callCtx(t), req)
			tc.assert(t, err)
			if err != nil {
				assertConstantText(t, err, "scripted", "/pool", importNFSURL, "connection reset")
			}
		})
	}
}

// TestRoutedImport_BudgetRanOutIsUnknown: a write phase cut short by the
// routed budget is an unknown outcome, never "not performed".
func TestRoutedImport_BudgetRanOutIsUnknown(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	prog := &importProgress{}
	prog.beginWrites()
	expired, ecancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer ecancel()
	err := classifyRoutedImportFailure("host-b", parent, expired, prog, ended1())
	assertUnknownOutcome(t, err)
	assert.Contains(t, status.Convert(err).Message(), `"host-b"`)

	// A budget too short to start the next command is the same answer.
	_, err = hostCommandTimeout(expired)
	assertUnknownOutcome(t, classifyRoutedImportFailure("host-b", parent, expired, prog, err))
}

// ended1 is a host command that ended with exit status 1.
func ended1() error { return virshFailure(1, "x") }

// TestRoutedImport_ReadPhaseFailuresAreDefinite pins the classification of the
// read phase directly: before any write nothing can have landed.
func TestRoutedImport_ReadPhaseFailuresAreDefinite(t *testing.T) {
	ctx := context.Background()
	fresh := &importProgress{}
	st := status.Convert(classifyRoutedImportFailure("host-b", ctx, ctx, fresh, virshFailure(-1, "reset")))
	assert.True(t, hasHostUnavailableInfo(st), "the host never answered")

	answered := &importProgress{}
	answered.record(ctx, false, nil)
	assertNotPerformedStatus(t, classifyRoutedImportFailure("host-b", ctx, ctx, answered, virshFailure(-1, "reset")))
	assertNotPerformedStatus(t, classifyRoutedImportFailure("host-b", ctx, ctx, answered, fmt.Errorf("parse: %w", ended1())))

	lease := &importProgress{}
	st = status.Convert(classifyRoutedImportFailure("host-b", ctx, ctx, lease, &hostOpError{host: "host-b",
		err: contracts.NewHostUnavailableError("connect to host", nil)}))
	assert.True(t, hasHostUnavailableInfo(st))
}

// TestImportProgress_CleanupIsNeverLost: a command of the cleanup connection
// (tagged routedImportConn.cleanup, never by the command's name) whose session
// drops does not make the outcome unknown; any write-phase command that did
// not end on its own does — an "rm" too.
func TestImportProgress_CleanupIsNeverLost(t *testing.T) {
	ctx := context.Background()
	rec := newImportRecHost()
	rec.libvirtConn = &fakeSeamConn{id: "host-b"}
	rec.failOn("rm", 0, virshFailure(-1, "reset"))
	p := &importProgress{}
	p.beginWrites()
	cleanup := &routedImportConn{libvirtConn: rec, progress: p, cleanup: true}
	_, err := cleanup.RunHost(ctx, "rm", "-rf", "--", "/pool/.virtrigaud-write-x")
	require.Error(t, err)
	_, _, lost := p.snapshot()
	assert.False(t, lost, "the cleanup connection's commands cannot land a disk")

	lock := hostLock{dir: "/s/" + hostLockDirName, name: "import-x.lock"}
	writer := &routedImportConn{libvirtConn: rec, progress: p, lock: &lock}
	rec.fail["rm"] = virshFailure(-1, "reset")
	rec.failN["rm"] = 1
	_, err = writer.RunHost(ctx, "rm", "-f", "--", "/pool/x")
	require.Error(t, err)
	_, _, lost = p.snapshot()
	assert.True(t, lost, "a guarded write-phase command is never exempt by its name")

	q := &importProgress{}
	q.beginWrites()
	q.record(ctx, true, virshFailure(1, "x"))
	_, _, lost = q.snapshot()
	assert.False(t, lost, "ended on its own")
	q.record(ctx, true, virshFailure(timeoutExpiredExit, ""))
	_, _, lost = q.snapshot()
	assert.True(t, lost, "stopped by timeout(1): B4 counts it as unknown")
	r := &importProgress{}
	r.record(ctx, false, virshFailure(-1, "reset"))
	_, _, lost = r.snapshot()
	assert.False(t, lost, "reads are never lost")
}

// TestStreamInError: a StreamIn failure carries the remote exit status, or -1
// for a session that failed, in the shape RunHost failures have.
func TestStreamInError(t *testing.T) {
	assert.NoError(t, streamInError(nil))
	assert.ErrorIs(t, streamInError(context.Canceled), context.Canceled)
	ve := virshFailure(3, "x")
	assert.Same(t, ve, streamInError(ve))
	var got *VirshError
	require.ErrorAs(t, streamInError(fmt.Errorf("ssh: session closed")), &got)
	assert.Equal(t, -1, got.ExitCode)
	assert.True(t, isHostTransportFailure(streamInError(fmt.Errorf("ssh: session closed"))))
}

// TestRoutedImport_LandingLock: the routed import holds the landing lock of
// the landing name, provider-wide (#415): a second import of the same name —
// on the same host or ANOTHER (a shared pool makes it the same file) — waits
// and gives up within its budget, retryably, having run nothing; a
// DeleteLandedDisk of the name through any host is answered "import in
// flight", never NOT_FOUND.
func TestRoutedImport_LandingLock(t *testing.T) {
	f := newImportFixture(t)
	unlock, err := landingLocks.lockForImport(context.Background(), routedLandingKey(importVolume))
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = f.server().ImportDisk(ctx, nfsRequest("host-b"))
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code(), "%v", err)
	assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason), "retried, out of the breaker")
	assert.Equal(t, wireRoutedImportLandingBusy, st.Message())
	assert.Empty(t, f.rec.snapshot(), "nothing ran while another import of the name holds it")

	for _, host := range []string{"host-a", "host-b"} {
		_, err = f.server().DeleteLandedDisk(context.Background(), &providerv1.DeleteLandedDiskRequest{
			TargetHostId: host, TargetVm: importTargetWire})
		assert.Equal(t, codes.Unavailable, status.Code(err), host)
		assert.Equal(t, landedDiskImportInFlightMessage, status.Convert(err).Message(), host)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, err = f.server().ImportDisk(ctx, nfsRequest(host))
		cancel()
		assert.Equal(t, wireRoutedImportLandingBusy, status.Convert(err).Message(),
			"the same landing name through %s is the same lock (a shared pool makes it the same file)", host)
	}
	assert.Equal(t, routedLandingKey(importVolume), importVolume, "the landing name alone, never the host")
	assert.Empty(t, f.rec.snapshot(), "nothing ran on any host")
}

// TestRoutedImport_ResendReplacesNeverAdds: a re-sent import (the manager
// lost the first answer) runs the same checks and lands at the same name with
// one rename: the earlier landing is replaced, never a second disk.
func TestRoutedImport_ResendReplacesNeverAdds(t *testing.T) {
	// One host: the second call finds the first call's disk and scans every
	// host of the Provider for a domain using it (none) before it replaces it.
	f := newImportFixtureOf(t, []string{"host-b"})
	first, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	require.NoError(t, err)
	f.rec.target = pathExistsMarker // the first call's disk is there now
	second, err := f.server().ImportDisk(callCtx(t), nfsRequest("host-b"))
	require.NoError(t, err)
	assert.Equal(t, first.GetPath(), second.GetPath())
	var publishes []string
	for _, c := range f.rec.snapshot() {
		if c.key() == "mv" {
			publishes = append(publishes, c.cmd[len(c.cmd)-1])
		}
	}
	assert.Equal(t, []string{importLanded, importLanded}, publishes, "each call publishes only onto the one landing name")
}
