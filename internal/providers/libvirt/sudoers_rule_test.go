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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the documented sudoers rule (docs/libvirt-clones.md, "What
// the copies run as root") to what the provider runs: every `sudo -n` command
// the clone, export, GetDiskInfo and chain-read paths run matches the rule
// exactly, and the shapes the provider never runs as root — a format probe,
// another format, another file, another NFS identity or option — do not.

// sudoersDoc is the document the rule is published in.
const sudoersDoc = "../../../docs/libvirt-clones.md"

// docSudoersRules returns the regular expressions of the sudoers snippet in
// sudoersDoc, by command base name, adjusted to the test fixtures: the NFS
// export nas:/e, and the fake id's uid and gid.
func docSudoersRules(t *testing.T) map[string][]*regexp.Regexp {
	t.Helper()
	b, err := os.ReadFile(sudoersDoc)
	require.NoError(t, err)
	doc := string(b)
	start := strings.Index(doc, "# /etc/sudoers.d/virtrigaud")
	require.GreaterOrEqual(t, start, 0, "the sudoers snippet is in %s", sudoersDoc)
	end := strings.Index(doc[start:], "```")
	require.Greater(t, end, 0)
	block := strings.ReplaceAll(doc[start:start+end], "\\\n", "")
	rules := map[string][]*regexp.Regexp{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Cmnd_Alias ") {
			continue
		}
		_, items, ok := strings.Cut(line, " = ")
		require.True(t, ok, line)
		for _, item := range strings.Split(items, ", ") {
			cmd, rx, ok := strings.Cut(strings.TrimSpace(item), " ")
			require.True(t, ok, item)
			require.True(t, strings.HasPrefix(rx, "^") && strings.HasSuffix(rx, "$"), "an anchored regular expression, never a wildcard: %q", item)
			rx = strings.ReplaceAll(rx, `nfs\.example\.com/exports/virtrigaud`, `nas/e`)
			rx = strings.ReplaceAll(rx, "uid=1001&gid=1001", "uid="+scdSSHUID+"&gid="+scdSSHGID)
			rules[filepath.Base(cmd)] = append(rules[filepath.Base(cmd)], regexp.MustCompile(rx))
		}
	}
	require.NotEmpty(t, rules["qemu-img"])
	require.NotEmpty(t, rules["timeout"])
	return rules
}

// sudoersAllows reports whether rules allow `sudo -n <cmd> <args>`.
func sudoersAllows(rules map[string][]*regexp.Regexp, command string) bool {
	cmd, args, _ := strings.Cut(command, " ")
	for _, rx := range rules[cmd] {
		if rx.MatchString(args) {
			return true
		}
	}
	return false
}

// sudoCommands returns what every `sudo -n` in calls ran.
func sudoCommands(calls []string) []string {
	var out []string
	for _, c := range calls {
		if cmd, ok := strings.CutPrefix(c, "local sudo -n "); ok {
			out = append(out, cmd)
		}
	}
	return out
}

func TestSudoersRule_AllowsExactlyWhatTheProviderRuns(t *testing.T) {
	rules := docSudoersRules(t)
	var emitted []string
	collect := func(calls []string) { emitted = append(emitted, sudoCommands(calls)...) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Single host: a full clone (chain read, root copy) and GetDiskInfo.
	for _, format := range []string{"qcow2", "raw"} {
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXMLAs(scdDomain, scdDomainOpts{}, format)})
		_, err := NewServer(fx.p).Clone(ctx, scdFullClone)
		require.NoError(t, err)
		_, err = NewServer(fx.p).GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{VmId: scdDomain})
		require.NoError(t, err)
		collect(fx.calls())
	}

	// Clustered: the clone of a snapshotted VM (a two-image chain) and the nfs
	// and s3 exports of a qcow2 and a raw disk, under the guard's timeout.
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": overlayDomainXML("web", scdDomainOpts{owner: ownerTeamA})}})
	fx.script("local", "backing-"+filepath.Base(scdOverlayPath), scdDiskPath)
	_, err := NewServer(fx.p).Clone(ctx, routedCloneReq())
	require.NoError(t, err)
	collect(fx.calls())
	for _, format := range []string{"qcow2", "raw"} {
		for _, backend := range []string{"nfs", "s3"} {
			fx := ownedWebOnB(t, scdDomainXMLAs("web", scdDomainOpts{owner: ownerTeamA}, format))
			fx.p.hostDiskTransportFn = anyTransport
			req := &providerv1.ExportDiskRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: backend,
				DestinationUrl: "nfs://nas/e/vmmigrations-team-a-m1-export.qcow2?nfsport=2049",
			}
			if backend == "s3" {
				req.DestinationUrl = "s3://bucket/web.qcow2"
				req.StorageOptionsJson = `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`
				req.Credentials = map[string]string{"accessKeyID": "a", "secretAccessKey": "s"}
			}
			_, _ = NewServer(fx.p).ExportDisk(ctx, req) // s3: no SSH stream after the flatten
			collect(fx.calls())
		}
	}

	var infos, clones, exports int
	for _, cmd := range emitted {
		assert.True(t, sudoersAllows(rules, cmd), "the documented rule allows what the provider runs as root: %q", cmd)
		switch {
		case strings.HasPrefix(cmd, "qemu-img info "):
			infos++
		case strings.Contains(cmd, "convert -U "):
			exports++
		case strings.Contains(cmd, "convert "):
			clones++
		}
	}
	assert.Positive(t, infos, "chain reads were checked")
	assert.Positive(t, clones, "clone copies were checked")
	assert.GreaterOrEqual(t, exports, 4, "nfs and s3 export copies of qcow2 and raw disks were checked")

	const img = "/var/lib/libvirt/images/web-disk.qcow2"
	const out = "/var/lib/libvirt/images/.virtrigaud-write-AbCdEfGhIj/team-a.copy-disk.qcow2"
	for _, cmd := range []string{
		"qemu-img info -U --output=json -- " + img,
		"qemu-img info -U -f vmdk --output=json -- " + img,
		"qemu-img info -U -f qcow2 --output=json -- /etc/shadow",
		"qemu-img info -U -f qcow2 --output=json -- /var/lib/libvirt/images/../../../etc/shadow",
		"qemu-img info -U -f qcow2 --output=json -- " + img + " " + img,
		"qemu-img info -U -f qcow2 --image-opts --output=json -- " + img,
		"qemu-img info -U -f qcow2 --backing-chain --output=json -- " + img,
		"qemu-img info -U *",
		"qemu-img convert -O qcow2 " + img + " " + out,
		"qemu-img convert -f vmdk -O qcow2 " + img + " " + out,
		"qemu-img convert -f qcow2 -O qcow2 " + img + " /etc/cron.d/x",
		"qemu-img convert -f qcow2 -O raw " + img + " " + out,
		"qemu-img convert -U -f qcow2 -O qcow2 " + img + " nfs://nas/e/x.qcow2",
		"qemu-img convert -U -f qcow2 -O qcow2 " + img + " nfs://nas/e/x.qcow2?uid=0&gid=0",
		"qemu-img convert -U -f qcow2 -O qcow2 " + img + " nfs://nas/e/x.qcow2?uid=" + scdSSHUID + "&gid=" + scdSSHGID + "&debug=9",
		"qemu-img convert -U -f qcow2 -O qcow2 " + img + " nfs://nas/e/x.qcow2?nfsport=1&uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		"qemu-img convert -U -f qcow2 -O qcow2 " + img + " nfs://evil/e/x.qcow2?uid=" + scdSSHUID + "&gid=" + scdSSHGID,
		"timeout --kill-after=10s 60s qemu-img info -U -f qcow2 --output=json -- " + img,
		"timeout --kill-after=10s 60s sh -c id",
		"sh -c id",
	} {
		assert.False(t, sudoersAllows(rules, cmd), "the documented rule refuses: %q", cmd)
	}
}

// chainReadQemuImg is a qemu-img in front of a fixture's that answers `info`
// for a three-image chain: $CHAIN_TOP backed by $CHAIN_L1 WITHOUT a backing
// format ($CHAIN_TOP_BACKING_FMT, empty by default), and $CHAIN_L1 backed by
// $CHAIN_L2 (qcow2). It records "root <args>" when run through
// chainReadSudo, "user <args>" otherwise, in $CHAIN_LOG.
const chainReadQemuImg = `#!/bin/sh
who=user
if [ "$CHAIN_AS_ROOT" = 1 ]; then who=root; fi
printf '%s %s\n' "$who" "$*" >> "$CHAIN_LOG"
for a in "$@"; do img="$a"; done
fmt=""
if [ -n "$CHAIN_TOP_BACKING_FMT" ]; then fmt=", \"backing-filename-format\": \"$CHAIN_TOP_BACKING_FMT\""; fi
case "$img" in
"$CHAIN_TOP") printf '{"filename": "%s", "format": "qcow2", "backing-filename": "%s", "full-backing-filename": "%s"%s}\n' "$img" "$CHAIN_L1" "$CHAIN_L1" "$fmt" ;;
"$CHAIN_L1") printf '{"filename": "%s", "format": "qcow2", "backing-filename": "%s", "full-backing-filename": "%s", "backing-filename-format": "qcow2"}\n' "$img" "$CHAIN_L2" "$CHAIN_L2" ;;
*) printf '{"filename": "%s", "format": "qcow2"}\n' "$img" ;;
esac
`

// chainReadSudo runs `sudo -n qemu-img ...` through chainReadQemuImg "as
// root"; it never runs the real sudo.
const chainReadSudo = `#!/bin/sh
if [ "$1" = -n ] && [ "$2" = qemu-img ]; then
  shift 2
  CHAIN_AS_ROOT=1 exec "$(dirname "$0")/qemu-img" "$@"
fi
echo "fake sudo: not a chain read" >&2
exit 1
`

// installChainRead installs chainReadQemuImg and chainReadSudo in front of the
// fixture's fakes and returns the file that records their reads.
func installChainRead(t *testing.T, top, l1, l2 string) string {
	t.Helper()
	for _, tool := range []string{"qemu-img", "sudo"} {
		p, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.Contains(t, p, os.TempDir(), "the fixture's fake %s must come first on PATH, never the real one", tool)
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(chainReadQemuImg), 0o755)) //nolint:gosec // test shim must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "sudo"), []byte(chainReadSudo), 0o755))        //nolint:gosec // test shim must be executable
	log := filepath.Join(t.TempDir(), "chain.log")
	t.Setenv("CHAIN_TOP", top)
	t.Setenv("CHAIN_L1", l1)
	t.Setenv("CHAIN_L2", l2)
	t.Setenv("CHAIN_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestWalkBackingChain_RootNeverProbes: root reads an image only in a qcow2
// or raw format the definition or a root-read header names; an image whose
// format nothing names, and every image below it, is read as the SSH user —
// so is an image of another format.
func TestWalkBackingChain_RootNeverProbes(t *testing.T) {
	const (
		top = "/var/lib/libvirt/images/web-disk.snap1"
		l1  = "/var/lib/libvirt/images/web-disk.qcow2"
		l2  = "/var/lib/libvirt/images/golden.qcow2"
	)
	h := localHostVP("host-b")
	ctx := context.Background()

	t.Run("an unnamed backing format", func(t *testing.T) {
		newRoutedSCD(t, nil)
		log := installChainRead(t, top, l1, l2)
		files, err := backingChainFiles(ctx, h, top, "qcow2")
		require.NoError(t, err)
		assert.Contains(t, files, l2, "the chain is still followed, for the in-use check")
		b, err := os.ReadFile(log) //nolint:gosec // test reads its own fixture log
		require.NoError(t, err)
		assert.Equal(t, []string{
			"root info -U -f qcow2 --output=json -- " + top,
			"user info -U --output=json -- " + l1,
			"user info -U -f qcow2 --output=json -- " + l2,
		}, splitLines(string(b)), "a probed header's backing file is never opened as root, even in a named format")
	})
	t.Run("another format", func(t *testing.T) {
		newRoutedSCD(t, nil)
		log := installChainRead(t, top, l1, l2)
		t.Setenv("CHAIN_TOP_BACKING_FMT", "vmdk")
		_, err := backingChainFiles(ctx, h, top, "qcow2")
		require.NoError(t, err)
		b, err := os.ReadFile(log) //nolint:gosec // test reads its own fixture log
		require.NoError(t, err)
		lines := splitLines(string(b))
		assert.Contains(t, lines, "user info -U -f vmdk --output=json -- "+l1)
		assert.Contains(t, lines, "root info -U -f qcow2 --output=json -- "+l2, "a qcow2 image a header read in its named format names")
	})
	t.Run("named formats throughout", func(t *testing.T) {
		newRoutedSCD(t, nil)
		log := installChainRead(t, top, l1, l2)
		t.Setenv("CHAIN_TOP_BACKING_FMT", "qcow2")
		_, err := backingChainFiles(ctx, h, top, "qcow2")
		require.NoError(t, err)
		b, err := os.ReadFile(log) //nolint:gosec // test reads its own fixture log
		require.NoError(t, err)
		for _, l := range splitLines(string(b)) {
			assert.True(t, strings.HasPrefix(l, "root info -U -f qcow2 --output=json -- "), "every read is the documented root shape: %q", l)
		}
	})
}
