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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin TransferOwner (ADR-0007 Addendum A, slice 4): the
// compare-and-swap owner re-stamp the clustered adoption flow — and A6's
// restore re-binding — uses so that the routed, owner-checked per-VM calls
// accept the VirtualMachine that takes a domain over.

// adopter is the VirtualMachine that takes the domain over in these tests.
var adopter = contracts.ObjectIdentity{UID: "5e1f0a2b-3c4d-4e5f-8a6b-7c8d9e0f1a2b", Namespace: "infra", Name: "web-3f2a9c1b"}

// staleOwner is a VirtualMachine the manager reports gone.
var staleOwner = contracts.ObjectIdentity{UID: "dead0000-0000-4000-8000-000000000000", Namespace: "team-a", Name: "web"}

// transferReq is the manager's TransferOwner request for "web" on host-a.
func transferReq(replaceable ...string) *providerv1.TransferOwnerRequest {
	return &providerv1.TransferOwnerRequest{
		Id:                   "web",
		TargetHostId:         "host-a",
		Owner:                &providerv1.ObjectIdentity{Uid: adopter.UID, Namespace: adopter.Namespace, Name: adopter.Name},
		ReplaceableOwnerUids: replaceable,
		ExpectedUuid:         uuidWebA,
	}
}

// metadataCalls returns the logged `virsh metadata` calls.
func metadataCalls(fx *listFixture) []string {
	var out []string
	for _, c := range fx.calls() {
		if strings.Contains(c, " metadata ") {
			out = append(out, c)
		}
	}
	return out
}

// hasVMOperationFailed reports whether err carries the VM_OPERATION_FAILED
// ErrorInfo.
func hasVMOperationFailed(err error) bool {
	st, _ := status.FromError(err)
	for _, d := range st.Details() {
		if ei, ok := d.(*errdetails.ErrorInfo); ok && ei.GetReason() == contracts.VMOperationFailedReason {
			return true
		}
	}
	return false
}

// TestClustered_TransferOwner_StampsUnstampedDomainAndRoutedCallsPassOwnerCheck:
// an unstamped domain (the single-host adoption policy adopts those) is
// stamped with the adopter's identity, on its UUID, persistent definition
// only while it is shut off. Before the transfer the adopter's routed
// Describe sees nothing; after it, Describe and Power pass the owner check.
func TestClustered_TransferOwner_StampsUnstampedDomainAndRoutedCallsPassOwnerCheck(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}, "host-b": {}})
	fx.scriptStamp("host-a", dom, adopter)
	p := clusterOf(t, []string{"host-a", "host-b"})
	ref := contracts.VMRef{ID: "web", HostID: "host-a", Owner: adopter}

	before, err := p.Describe(context.Background(), ref)
	require.NoError(t, err)
	assert.False(t, before.Exists, "an unstamped domain is invisible to the adopter until its owner is transferred")

	_, err = NewServer(p).TransferOwner(context.Background(), transferReq())
	require.NoError(t, err)

	assert.Equal(t, []string{
		"host-a metadata " + uuidWebA + " --uri " + ownerMetadataNamespaceURI + " --key " + ownerMetadataPrefix +
			" --set <owner uid='" + adopter.UID + "' namespace='infra' name='web-3f2a9c1b'/> --config",
	}, metadataCalls(fx), "one stamp, addressed by UUID, persistent definition only (the domain is shut off)")
	owners, err := domainOwners(fx.read("host-a", "dom-web.xml"))
	require.NoError(t, err)
	assert.Equal(t, []contracts.ObjectIdentity{adopter}, owners, "uid, namespace and name are all the new owner's")

	after, err := p.Describe(context.Background(), ref)
	require.NoError(t, err)
	assert.True(t, after.Exists, "the adopter's routed Describe passes the owner check")
	_, err = p.Power(context.Background(), ref, contracts.PowerOpOff)
	require.NoError(t, err, "the adopter's routed Power passes the owner check")

	for _, c := range fx.calls() {
		assert.True(t, strings.HasPrefix(c, "host-a "), "only the named host is touched: %s", c)
	}
	assert.Zero(t, p.virshProvider.unroutableHits.Load())
}

// TestClustered_TransferOwner_ActiveDomainStampsBothDefinitions: a running
// domain is stamped live and persistently, and both are read back.
func TestClustered_TransferOwner_ActiveDomainStampsBothDefinitions(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA, state: "running"}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.scriptStamp("host-a", dom, adopter)
	p := clusterOf(t, []string{"host-a"})

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
	require.NoError(t, err)
	calls := metadataCalls(fx)
	require.Len(t, calls, 1)
	assert.True(t, strings.HasSuffix(calls[0], " --config --live"), calls[0])
	assert.Contains(t, fx.calls(), "host-a dumpxml --inactive "+uuidWebA, "the persistent definition is verified too")
}

// TestClustered_TransferOwner_ReplacesAStaleStamp: a domain stamped by a
// VirtualMachine the manager reports gone is re-stamped.
func TestClustered_TransferOwner_ReplacesAStaleStamp(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA, owner: staleOwner}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.scriptStamp("host-a", dom, adopter)
	p := clusterOf(t, []string{"host-a"})

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq(staleOwner.UID))
	require.NoError(t, err)
	owners, err := domainOwners(fx.read("host-a", "dom-web.xml"))
	require.NoError(t, err)
	assert.Equal(t, []contracts.ObjectIdentity{adopter}, owners)
}

// TestClustered_TransferOwner_RefusesWithoutTouching covers every refusal of
// the compare-and-swap: nothing is stamped in any of them.
func TestClustered_TransferOwner_RefusesWithoutTouching(t *testing.T) {
	twoStamps := func(fx *listFixture) {
		doc := listDomainDoc(listDomain{name: "web", uuid: uuidWebA})
		doc = strings.Replace(doc, "<memory", "  <metadata>\n    "+renderOwnerElementXML(staleOwner)+"\n    "+
			renderOwnerElementXML(ownerTeamB)+"\n  </metadata>\n  <memory", 1)
		fx.write("host-a", "dom-web.xml", doc)
	}
	unreadable := func(fx *listFixture) {
		doc := listDomainDoc(listDomain{name: "web", uuid: uuidWebA})
		doc = strings.Replace(doc, "<memory", "  <metadata>\n    <virtrigaud:owner xmlns:virtrigaud='"+
			ownerMetadataNamespaceURI+"' uid='a' uid='b'/>\n  </metadata>\n  <memory", 1)
		fx.write("host-a", "dom-web.xml", doc)
	}
	for _, tc := range []struct {
		name  string
		dom   listDomain
		setup func(*listFixture)
		req   *providerv1.TransferOwnerRequest
		code  codes.Code
	}{
		{name: "stamped for a live VirtualMachine", dom: listDomain{name: "web", uuid: uuidWebA, owner: ownerTeamA},
			req: transferReq(staleOwner.UID), code: codes.AlreadyExists},
		{name: "stamped, nothing replaceable", dom: listDomain{name: "web", uuid: uuidWebA, owner: staleOwner},
			req: transferReq(), code: codes.AlreadyExists},
		{name: "two stamps", dom: listDomain{name: "web", uuid: uuidWebA}, setup: twoStamps,
			req: transferReq(staleOwner.UID, ownerTeamB.UID), code: codes.AlreadyExists},
		{name: "unreadable stamp", dom: listDomain{name: "web", uuid: uuidWebA}, setup: unreadable,
			req: transferReq("a", "b"), code: codes.AlreadyExists},
		{name: "replaced since it was listed", dom: listDomain{name: "web", uuid: uuidWebB},
			req: transferReq(), code: codes.NotFound},
		{name: "gone from the host", dom: listDomain{name: "other", uuid: uuidWebA},
			req: transferReq(), code: codes.NotFound},
		{name: "no owner uid", dom: listDomain{name: "web", uuid: uuidWebA},
			req: func() *providerv1.TransferOwnerRequest { r := transferReq(); r.Owner = nil; return r }(), code: codes.InvalidArgument},
		{name: "no expected uuid", dom: listDomain{name: "web", uuid: uuidWebA},
			req: func() *providerv1.TransferOwnerRequest { r := transferReq(); r.ExpectedUuid = ""; return r }(), code: codes.InvalidArgument},
		{name: "no host", dom: listDomain{name: "web", uuid: uuidWebA},
			req: func() *providerv1.TransferOwnerRequest { r := transferReq(); r.TargetHostId = ""; return r }(), code: codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newListFixture(t, map[string][]listDomain{"host-a": {tc.dom}})
			if tc.setup != nil {
				tc.setup(fx)
			}
			fx.scriptStamp("host-a", tc.dom, adopter)
			p := clusterOf(t, []string{"host-a"})

			_, err := NewServer(p).TransferOwner(context.Background(), tc.req)
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err), "%v", err)
			assert.Empty(t, metadataCalls(fx), "a refused transfer never stamps")
			assert.NotContains(t, status.Convert(err).Message(), ownerTeamA.Namespace,
				"the refusal never names the other owner")
		})
	}
}

// TestClustered_TransferOwner_IdempotentRetry: a domain that already carries
// the adopter's stamp succeeds without another write.
func TestClustered_TransferOwner_IdempotentRetry(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA, owner: adopter}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	p := clusterOf(t, []string{"host-a"})

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
	require.NoError(t, err)
	assert.Empty(t, metadataCalls(fx), "already the adopter's: nothing is written")
}

// TestClustered_TransferOwner_StampThatDidNotTakeFails: a metadata write that
// reports success but leaves the domain without the adopter's stamp is never
// reported as a transfer (the manager does not bind).
func TestClustered_TransferOwner_StampThatDidNotTakeFails(t *testing.T) {
	fx := newListFixture(t, map[string][]listDomain{"host-a": {{name: "web", uuid: uuidWebA}}})
	p := clusterOf(t, []string{"host-a"})

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
	require.Error(t, err)
	assert.Equal(t, codes.Unknown, status.Code(err))
	assert.True(t, hasVMOperationFailed(err), "a per-VM failure the breaker ignores: %v", err)
	assert.Len(t, metadataCalls(fx), 1)
}

// TestClustered_TransferOwner_FailuresAreRoutedErrors: a failed stamp is a
// sanitized VM_OPERATION_FAILED, an unreachable host HOST_UNAVAILABLE.
func TestClustered_TransferOwner_FailuresAreRoutedErrors(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	fx.write("host-a", "fail-metadata", "")
	p := clusterOf(t, []string{"host-a", "host-b"}, "host-b")

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
	require.Error(t, err)
	assert.True(t, hasVMOperationFailed(err), "%v", err)
	assert.NotContains(t, err.Error(), "scripted failure", "host output never reaches the wire")

	req := transferReq()
	req.TargetHostId = "host-b"
	_, err = NewServer(p).TransferOwner(context.Background(), req)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	st, _ := status.FromError(err)
	require.NotEmpty(t, st.Details())
	ei, ok := st.Details()[0].(*errdetails.ErrorInfo)
	require.True(t, ok)
	assert.Equal(t, contracts.HostUnavailableReason, ei.GetReason())
	assert.NotContains(t, err.Error(), "qemu+ssh", "the host's SSH endpoint never reaches the wire")
}

// TestOwnerTransferDecision pins the compare-and-swap decision table.
func TestOwnerTransferDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded []contracts.ObjectIdentity
		readErr  error
		replace  []string
		already  bool
		refused  bool
	}{
		{name: "unstamped", already: false},
		{name: "already the owner's", recorded: []contracts.ObjectIdentity{adopter}, already: true},
		{name: "stale and replaceable", recorded: []contracts.ObjectIdentity{staleOwner}, replace: []string{staleOwner.UID}},
		{name: "stamped, not replaceable", recorded: []contracts.ObjectIdentity{staleOwner}, refused: true},
		{name: "live owner, other uid replaceable", recorded: []contracts.ObjectIdentity{ownerTeamA}, replace: []string{staleOwner.UID}, refused: true},
		{name: "two stamps", recorded: []contracts.ObjectIdentity{staleOwner, adopter}, replace: []string{staleOwner.UID}, refused: true},
		{name: "stamp without uid", recorded: []contracts.ObjectIdentity{{Namespace: "x", Name: "y"}}, replace: []string{""}, refused: true},
		{name: "unreadable", readErr: assert.AnError, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			already, err := ownerTransferDecision(adopter, tc.recorded, tc.readErr, tc.replace)
			if tc.refused {
				assert.Error(t, err)
				assert.False(t, already)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.already, already)
		})
	}
}

// TestClustered_TransferOwner_ActiveDomainPersistentStampIsCheckedToo: a
// running domain whose persistent definition carries another VirtualMachine's
// stamp (its live definition none) is refused: both definitions are stamped,
// so both must pass the compare-and-swap.
func TestClustered_TransferOwner_ActiveDomainPersistentStampIsCheckedToo(t *testing.T) {
	dom := listDomain{name: "web", uuid: uuidWebA, state: "running"}
	fx := newListFixture(t, map[string][]listDomain{"host-a": {dom}})
	persisted := dom
	persisted.owner = ownerTeamA
	fx.write("host-a", "dom-"+uuidWebA+".xml", listDomainDoc(persisted)) // what `dumpxml --inactive <uuid>` reads
	fx.scriptStamp("host-a", dom, adopter)
	p := clusterOf(t, []string{"host-a"})

	_, err := NewServer(p).TransferOwner(context.Background(), transferReq())
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.Empty(t, metadataCalls(fx))
}

// TestHostLocks_PerHostAndContextAware: transfers on one host wait for each
// other, a wait gives up with its context, and another host is not held up.
func TestHostLocks_PerHostAndContextAware(t *testing.T) {
	var locks hostLocks
	unlockA, err := locks.lock(context.Background(), "host-a")
	require.NoError(t, err)

	unlockB, err := locks.lock(context.Background(), "host-b")
	require.NoError(t, err, "another host is not held up")
	unlockB()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = locks.lock(ctx, "host-a")
	require.ErrorIs(t, err, context.DeadlineExceeded, "a wait on a busy host gives up with its context")

	unlockA()
	unlockA2, err := locks.lock(context.Background(), "host-a")
	require.NoError(t, err, "released: the next transfer on the host proceeds")
	unlockA2()
}
