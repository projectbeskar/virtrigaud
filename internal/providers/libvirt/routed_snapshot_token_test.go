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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin that a clustered SnapshotCreate is idempotent per REQUEST,
// not per name (routed_snapshot_token.go): a retry adopts only the snapshot
// its own earlier attempt made (recorded request token), never a same-named
// leftover of another request.

// answerSnapshotDescription puts a virsh in front of the routed fakes that
// answers `snapshot-dumpxml` (logged like any virsh call) with a snapshot
// document whose description is desc, and hands every other call on.
func answerSnapshotDescription(t *testing.T, desc string) {
	t.Helper()
	t.Setenv("FAKE_SNAP_DESC", desc)
	script := "#!/bin/sh\ncase \"$*\" in *snapshot-dumpxml*)\n" +
		"  host=local; if [ \"$1\" = -c ]; then host=\"${2##*/}\"; shift 2; fi\n" +
		"  printf '%s virsh %s\\n' \"$host\" \"$*\" >> \"$FAKE_SCD_DIR/calls.log\"\n" +
		"  printf '<domainsnapshot><name>%s</name><description>%s</description></domainsnapshot>\\n' \"$3\" \"$FAKE_SNAP_DESC\"; exit 0 ;;\nesac\n" +
		"exec \"$FAKE_SCD_BIN/virsh\" \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// tokenMarker is the recorded form of token in a snapshot's description.
func tokenMarker(token string) string {
	return snapshotRequestTokenPrefix + token + snapshotRequestTokenSuffix
}

// TestClustered_SnapshotCreate_IdempotentPerRequestNotPerName: an existing
// snapshot of the requested name is adopted only when it records this
// request's token. A leftover of another request (another token, no token,
// or a request without one) is ALREADY_EXISTS — never adopted, never
// replaced — so a new VMSnapshot can never report Ready on an older snapshot.
func TestClustered_SnapshotCreate_IdempotentPerRequestNotPerName(t *testing.T) {
	for name, tc := range map[string]struct {
		recorded, requested string
	}{
		"another request's token":         {"nightly " + tokenMarker("uid-old"), "uid-new"},
		"no recorded token (a leftover)":  {"nightly", "uid-new"},
		"a request without a token":       {"nightly " + tokenMarker("uid-old"), ""},
		"neither records nor sends token": {"nightly", ""},
	} {
		t.Run(name, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})) // snapshots s1, s2
			answerSnapshotDescription(t, tc.recorded)
			_, err := NewServer(fx.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "s2", RequestToken: tc.requested,
			})
			assert.Equal(t, codes.AlreadyExists, status.Code(err), "got %v", err)
			assert.Contains(t, err.Error(), `a snapshot named "s2" already exists on this VM and was not made for this request`)
			for _, c := range fx.calls() {
				for _, verb := range scdMutatingSnapshotVerbs {
					assert.NotContains(t, c, verb, "the leftover is neither adopted nor replaced: %q", c)
				}
			}
		})
	}
}

// TestClustered_SnapshotCreate_RecordsTheRequestToken: a created snapshot
// records the request's token at the end of its description; a token that is
// not an identifier is refused before any host is touched.
func TestClustered_SnapshotCreate_RecordsTheRequestToken(t *testing.T) {
	uuid := routingDomainUUID
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err := NewServer(fx.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "pre", Description: "before upgrade", RequestToken: "uid-snap-1",
	})
	require.NoError(t, err)
	assert.Contains(t, fx.calls(), "host-b virsh snapshot-create-as "+uuid+" pre --description before upgrade "+
		tokenMarker("uid-snap-1")+" --atomic --disk-only")

	fx2 := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	_, err = NewServer(fx2.p).SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, NameHint: "bad", RequestToken: "uid ] [x",
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a token that is not an identifier is refused: %v", err)
	assert.Empty(t, fx2.calls(), "no host is touched")
}

func TestSnapshotRequestToken(t *testing.T) {
	for desc, want := range map[string]string{
		"":                          "",
		"nightly":                   "",
		"d " + tokenMarker("abc-1"): "abc-1",
		tokenMarker("old") + " " + tokenMarker("new"): "new",
		"d " + snapshotRequestTokenPrefix + "open":    "",
	} {
		assert.Equal(t, want, snapshotRequestToken(desc), "%q", desc)
	}
	req := withSnapshotRequestToken(&providerv1.SnapshotCreateRequest{RequestToken: "t-1"})
	assert.True(t, strings.HasPrefix(req.GetDescription(), "Snapshot created by VirtRigaud at "), "the default description keeps its text")
	assert.Equal(t, "t-1", snapshotRequestToken(req.GetDescription()))
	plain := &providerv1.SnapshotCreateRequest{Description: "x"}
	assert.Same(t, plain, withSnapshotRequestToken(plain), "no token: unchanged")
}
