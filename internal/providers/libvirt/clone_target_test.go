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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin rc2 R4 on a single libvirt host: a Clone that names its
// target VirtualMachine's UID stamps the clone's domain with it, a re-sent
// Clone reports that domain instead of copying again, and a same-named domain
// the target does not own is refused with a Conflict, as for a Create.

// shCloneDomain is the domain scdStampedClone names: "<namespace>.<name>"
// of scdTargetVM.
const shCloneDomain = "team-a.copy"

// shCloneTarget is the target VirtualMachine of scdStampedClone, with its UID.
var shCloneTarget = contracts.ObjectIdentity{UID: "5f1d7c1e-2b9a-4e0f-8c3d-6a7b8c9d0e1f", Namespace: "team-a", Name: "copy"}

// scdStampedClone full-clones scdDomain for shCloneTarget (a manager that sends
// the target VirtualMachine's UID).
func scdStampedClone() *providerv1.CloneRequest {
	return &providerv1.CloneRequest{SourceVmId: scdDomain, TargetName: shCloneTarget.Name,
		TargetVm: &providerv1.ObjectIdentity{Uid: shCloneTarget.UID, Namespace: shCloneTarget.Namespace, Name: shCloneTarget.Name}}
}

// TestServer_AdvertisesOwnerRebind: a single-host and a clustered libvirt
// provider both bind a domain stamped with the same owner on a re-sent Create
// or Clone, and report it (rc2 R4).
func TestServer_AdvertisesOwnerRebind(t *testing.T) {
	caps, err := (&Server{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerRebind())
	assert.True(t, clusteredCapabilities().GetSupportsOwnerRebind())
}

func TestSingleHostClone_StampsTheCloneWithItsTarget(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{owner: ownerTeamA})})

	resp, err := NewServer(fx.p).Clone(context.Background(), scdStampedClone())
	require.NoError(t, err)
	assert.Equal(t, shCloneDomain, resp.TargetVmId)

	owners, err := domainOwners(fx.definedXML(shCloneDomain))
	require.NoError(t, err)
	assert.Equal(t, []contracts.ObjectIdentity{shCloneTarget}, owners,
		"the clone carries its target VirtualMachine's stamp, never the source's")
}

func TestSingleHostClone_WithoutUIDIsUnstamped(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{owner: ownerTeamA})})

	_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
	require.NoError(t, err)
	owners, err := domainOwners(fx.definedXML(shCloneDomain))
	require.NoError(t, err)
	assert.Empty(t, owners, "a manager older than the stamp gets today's unstamped clone")
}

// TestSingleHostClone_ResendBindsTheStampedClone: the domain an earlier
// attempt defined is reported as the clone — even though the source is now
// running, which a new copy would refuse — and nothing is read for a copy,
// copied or defined.
func TestSingleHostClone_ResendBindsTheStampedClone(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{
		scdDomain:     scdDomainXML(scdDomain, scdDomainOpts{}),
		shCloneDomain: scdDomainXML(shCloneDomain, scdDomainOpts{owner: shCloneTarget}),
	})
	fx.script("single", "state", "running\n")

	resp, err := NewServer(fx.p).Clone(context.Background(), scdStampedClone())
	require.NoError(t, err)
	assert.Equal(t, shCloneDomain, resp.TargetVmId)
	assert.Equal(t, []string{"single virsh list --all", "single virsh dumpxml " + shCloneDomain}, fx.calls(),
		"only the target's stamp is read")
	assert.Empty(t, fx.definedXML(shCloneDomain), "nothing is defined")
}

// TestSingleHostClone_RefusesADomainItDoesNotOwn: a same-named domain that is
// unstamped, stamped for another VirtualMachine, or met by a request without
// a UID is never bound or redefined: AlreadyExists, before anything is read
// for a copy.
func TestSingleHostClone_RefusesADomainItDoesNotOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		existing string
		req      *providerv1.CloneRequest
	}{
		"unstamped":             {scdDomainXML(shCloneDomain, scdDomainOpts{}), scdStampedClone()},
		"another VM's stamp":    {scdDomainXML(shCloneDomain, scdDomainOpts{owner: ownerTeamB}), scdStampedClone()},
		"a request with no uid": {scdDomainXML(shCloneDomain, scdDomainOpts{owner: shCloneTarget}), scdFullClone},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newSCDFixture(t, map[string]string{
				scdDomain:     scdDomainXML(scdDomain, scdDomainOpts{}),
				shCloneDomain: tc.existing,
			})
			_, err := NewServer(fx.p).Clone(context.Background(), tc.req)
			require.Error(t, err)
			st := status.Convert(err)
			assert.Equal(t, codes.AlreadyExists, st.Code(), "the class a Create uses: %v", err)
			assert.Contains(t, st.Message(), "is not owned by the clone's target VirtualMachine")
			assert.NotContains(t, st.Message(), ownerTeamB.UID, "the other owner is never named")
			assert.Equal(t, []string{"single virsh list --all", "single virsh dumpxml " + shCloneDomain}, fx.calls(),
				"nothing is read for a copy")
			assert.Empty(t, fx.definedXML(shCloneDomain), "nothing is defined")
		})
	}
}

// TestSingleHostClone_ResendWaitsForTheRunningAttempt: the target name's lock
// is held from the target check to the define, so a re-send that arrives
// while an earlier attempt still copies waits for it. One that cannot get
// the lock within its budget is answered retryable, with nothing done.
func TestSingleHostClone_ResendWaitsForTheRunningAttempt(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	unlock, err := fx.p.domainLocks.lock(context.Background(), shCloneDomain)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = NewServer(fx.p).Clone(ctx, scdStampedClone())
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	assert.Contains(t, errorInfoReasonList(status.Convert(err)), contracts.VMOperationFailedReason,
		"a per-VM wait the manager retries, never counted by its breaker")
	assert.Empty(t, fx.calls(), "nothing runs on the host")
}
