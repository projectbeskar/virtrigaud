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

package imageartifact

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// identityRequest returns a parsed identity request for team-a/ubuntu.
func identityRequest() Request {
	return Request{
		Mode:         ModeIdentity,
		Image:        testImage("team-a", "ubuntu"),
		SourceDigest: testDigest,
		PreparedBy:   contracts.ObjectIdentity{UID: "prov-uid", Namespace: "team-a", Name: "libvirt"},
	}
}

// TestNewStamp verifies a stamp records the request identity, the Provider
// and an RFC 3339 UTC time, and serializes with the documented field names
// (the libvirt sidecar form, ADR-0009 per-provider mapping).
func TestNewStamp(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	s := NewStamp(identityRequest(), at)
	require.NoError(t, s.Validate())
	assert.Equal(t, StampVersion, s.StampVersion)
	assert.Equal(t, testImage("team-a", "ubuntu"), s.Image.Identity())
	assert.Equal(t, testDigest, s.SourceDigest)
	assert.Equal(t, contracts.ObjectIdentity{UID: "prov-uid", Namespace: "team-a", Name: "libvirt"}, s.PreparedBy.Identity())
	assert.Equal(t, "2026-09-25T10:00:00Z", s.PreparedAt)

	raw, err := json.Marshal(s)
	require.NoError(t, err)
	assert.JSONEq(t, `{"stampVersion":1,
		"image":{"uid":"`+testUID+`","namespace":"team-a","name":"ubuntu"},
		"sourceDigest":"`+testDigest+`",
		"preparedBy":{"uid":"prov-uid","namespace":"team-a","name":"libvirt"},
		"preparedAt":"2026-09-25T10:00:00Z"}`, string(raw))
}

// TestStampValidate verifies an unknown version, a malformed UID or a
// malformed digest make a stamp untrusted.
func TestStampValidate(t *testing.T) {
	good := NewStamp(identityRequest(), time.Now())
	for name, mutate := range map[string]func(*Stamp){
		"unknown version":  func(s *Stamp) { s.StampVersion = 2 },
		"zero version":     func(s *Stamp) { s.StampVersion = 0 },
		"empty uid":        func(s *Stamp) { s.Image.UID = "" },
		"malformed uid":    func(s *Stamp) { s.Image.UID = "a/b" },
		"truncated digest": func(s *Stamp) { s.SourceDigest = s.SourceDigest[:20] },
		"empty digest":     func(s *Stamp) { s.SourceDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := good
			mutate(&s)
			assert.Error(t, s.Validate())
			assert.False(t, s.Matches(identityRequest()), "an untrusted stamp never matches")
		})
	}
}

// TestDecide pins the ADR-0009 D4 decision table.
func TestDecide(t *testing.T) {
	req := identityRequest()
	match := NewStamp(req, time.Now())
	otherUID := match
	otherUID.Image.UID = "0d4e7c1a-9f3b-4c2d-8e5a-6b7c8d9e0f1a"
	otherDigest := match
	otherDigest.SourceDigest = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	untrusted := match
	untrusted.StampVersion = 99
	// Only the UID and digest are authoritative: another namespace, name or
	// preparedBy on a stamp with the same UID and digest still matches.
	sameIdentityOtherAudit := match
	sameIdentityOtherAudit.Image.Namespace, sameIdentityOtherAudit.Image.Name = "team-b", "other"
	sameIdentityOtherAudit.PreparedBy = StampIdentity{UID: "p2", Namespace: "team-b", Name: "vsphere"}

	for name, tc := range map[string]struct {
		obs  Observation
		want Outcome
	}{
		"nothing at the name":                    {Observation{}, OutcomeImport},
		"complete, matching stamp":               {Observation{Exists: true, Complete: true, Stamp: &match}, OutcomeReuse},
		"complete, matching UID+digest only":     {Observation{Exists: true, Complete: true, Stamp: &sameIdentityOtherAudit}, OutcomeReuse},
		"incomplete, matching stamp, live":       {Observation{Exists: true, Live: true, Stamp: &match}, OutcomeInProgress},
		"incomplete, matching stamp, not live":   {Observation{Exists: true, Stamp: &match}, OutcomeAbandoned},
		"complete, no stamp (legacy or planted)": {Observation{Exists: true, Complete: true}, OutcomeConflict},
		"incomplete, no stamp":                   {Observation{Exists: true, Live: true}, OutcomeConflict},
		"complete, another UID":                  {Observation{Exists: true, Complete: true, Stamp: &otherUID}, OutcomeConflict},
		"complete, another digest":               {Observation{Exists: true, Complete: true, Stamp: &otherDigest}, OutcomeConflict},
		"complete, untrusted stamp":              {Observation{Exists: true, Complete: true, Stamp: &untrusted}, OutcomeConflict},
		"incomplete, another UID, not live":      {Observation{Exists: true, Stamp: &otherUID}, OutcomeConflict},
		"incomplete, untrusted stamp, not live":  {Observation{Exists: true, Stamp: &untrusted}, OutcomeConflict},
		"complete, matching stamp, live ignored": {Observation{Exists: true, Complete: true, Live: true, Stamp: &match}, OutcomeReuse},
		"incomplete, another digest, live":       {Observation{Exists: true, Live: true, Stamp: &otherDigest}, OutcomeConflict},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Decide(tc.obs, req))
		})
	}

	// A request without an image UID (never an identity request) matches no stamp.
	assert.Equal(t, OutcomeConflict, Decide(Observation{Exists: true, Complete: true, Stamp: &match}, Request{SourceDigest: testDigest}))
}

// TestOutcomeString pins the outcome names.
func TestOutcomeString(t *testing.T) {
	for o, want := range map[Outcome]string{
		OutcomeImport: "import", OutcomeReuse: "reuse", OutcomeInProgress: "in_progress",
		OutcomeAbandoned: "abandoned", OutcomeConflict: "conflict", Outcome(0): "Outcome(0)",
	} {
		assert.Equal(t, want, o.String())
	}
}

// TestStampPreparedArtifact verifies the response echo reports the stamp's
// identity, the artifact name and whether it was reused.
func TestStampPreparedArtifact(t *testing.T) {
	s := NewStamp(identityRequest(), time.Now())
	a := s.PreparedArtifact("team-a.ubuntu_0123456789abcdef", true)
	assert.Equal(t, "team-a.ubuntu_0123456789abcdef", a.GetName())
	assert.Equal(t, testUID, a.GetImage().GetUid())
	assert.Equal(t, "team-a", a.GetImage().GetNamespace())
	assert.Equal(t, "ubuntu", a.GetImage().GetName())
	assert.Equal(t, testDigest, a.GetSourceDigest())
	assert.True(t, a.GetReused())
}
