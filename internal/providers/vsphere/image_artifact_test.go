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

package vsphere

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Identities and digests the ADR-0009 vSphere tests use.
const (
	testImageUID    = "5f0c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	testOtherUID    = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	testProviderUID = "7e6d5c4b-3a29-4817-9f6e-5d4c3b2a1908"
)

var (
	testDigestA = "sha256:" + strings.Repeat("a", 64)
	testDigestB = "sha256:" + strings.Repeat("b", 64)
)

// testIdentity is the identity request of VMImage team-a/ubuntu (testImageUID)
// for source digest digest.
func testIdentity(digest string) imageartifact.Request {
	return imageartifact.Request{
		Mode:         imageartifact.ModeIdentity,
		Image:        contracts.ObjectIdentity{UID: testImageUID, Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: digest,
		PreparedBy:   contracts.ObjectIdentity{UID: testProviderUID, Namespace: "team-a", Name: "vsphere"},
	}
}

// stampAt is the stamp testIdentity(digest) writes, prepared at t.
func stampAt(digest string, t time.Time) *imageartifact.Stamp {
	s := imageartifact.NewStamp(testIdentity(digest), t)
	return &s
}

func TestImageStampExtraConfigRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	s := stampAt(testDigestA, at)

	ec := imageStampExtraConfig(*s)
	assert.Equal(t, imageStampKeys, optionKeys(ec), "every stamp key, in a fixed order")
	for _, bov := range ec {
		assert.True(t, strings.HasPrefix(bov.GetOptionValue().Key, reservedExtraConfigPrefix),
			"the stamp lives in the reserved prefix that stripReservedExtraConfig removes from OVFs")
	}

	got, err := imageStampFromExtraConfig(append([]types.BaseOptionValue{optVal("guestinfo.x", "y")}, ec...))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, *s, *got)
	assert.True(t, got.Matches(testIdentity(testDigestA)))

	t.Run("no provider identity leaves preparedby out", func(t *testing.T) {
		req := testIdentity(testDigestA)
		req.PreparedBy = contracts.ObjectIdentity{}
		noBy := imageartifact.NewStamp(req, at)
		ec := imageStampExtraConfig(noBy)
		assert.NotContains(t, optionKeys(ec), imageStampKeyPreparedBy)
		got, err := imageStampFromExtraConfig(ec)
		require.NoError(t, err)
		assert.Equal(t, noBy, *got)
	})
}

func TestImageStampFromExtraConfig_AbsentOrCleared(t *testing.T) {
	for name, ec := range map[string][]types.BaseOptionValue{
		"nil":            nil,
		"no stamp keys":  {optVal("guestinfo.userdata", "x"), optVal(ownerExtraConfigKeyUID, testImageUID)},
		"cleared stamp":  clearedImageStampExtraConfig(),
		"nil entries":    {nil, optVal("a", "b")},
		"only other ns.": {optVal("virtrigaud.imagex.uid", testImageUID)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := imageStampFromExtraConfig(ec)
			assert.NoError(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestImageStampFromExtraConfig_FailsClosed(t *testing.T) {
	good := imageStampExtraConfig(*stampAt(testDigestA, time.Now()))
	with := func(key string, value any) []types.BaseOptionValue {
		out := make([]types.BaseOptionValue, 0, len(good))
		for _, bov := range good {
			if bov.GetOptionValue().Key == key {
				out = append(out, &types.OptionValue{Key: key, Value: value})
				continue
			}
			out = append(out, bov)
		}
		return out
	}
	for name, ec := range map[string][]types.BaseOptionValue{
		"repeated key":             append(good, optVal(imageStampKeyUID, testImageUID)),
		"repeated key, other case": append(good, optVal("VirtRigaud.Image.UID", testOtherUID)),
		"non-string value":         with(imageStampKeyUID, 42),
		"unknown version":          with(imageStampKeyVersion, "2"),
		"non-numeric version":      with(imageStampKeyVersion, "one"),
		"missing version":          with(imageStampKeyVersion, ""),
		"truncated digest":         with(imageStampKeySourceDigest, testDigestA[:40]),
		"uppercase digest":         with(imageStampKeySourceDigest, strings.ToUpper(testDigestA)),
		"implausible uid":          with(imageStampKeyUID, "uid with spaces"),
		"missing uid":              with(imageStampKeyUID, ""),
		"unparseable preparedat":   with(imageStampKeyPreparedAt, "yesterday"),
		"missing preparedat":       with(imageStampKeyPreparedAt, ""),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := imageStampFromExtraConfig(ec)
			assert.Error(t, err)
			assert.Nil(t, got, "an untrusted stamp is the same as no stamp")
		})
	}
}

func TestClearedImageStampExtraConfig(t *testing.T) {
	ec := clearedImageStampExtraConfig()
	assert.Equal(t, imageStampKeys, optionKeys(ec))
	for _, bov := range ec {
		assert.Equal(t, "", bov.GetOptionValue().Value, "vSphere removes a key set to the empty string")
	}
}

func TestArtifactStalenessBound(t *testing.T) {
	for in, want := range map[string]time.Duration{
		`{}`:                                2 * time.Hour,
		`not json`:                          2 * time.Hour,
		`{"prepare":{}}`:                    2 * time.Hour,
		`{"prepare":{"timeout":"30m0s"}}`:   2 * time.Hour,
		`{"prepare":{"timeout":"1h"}}`:      2 * time.Hour,
		`{"prepare":{"timeout":"90m"}}`:     3 * time.Hour,
		`{"prepare":{"timeout":"6h"}}`:      12 * time.Hour,
		`{"prepare":{"timeout":"-5m"}}`:     2 * time.Hour,
		`{"prepare":{"timeout":"garbage"}}`: 2 * time.Hour,
	} {
		assert.Equal(t, want, artifactStalenessBound(in), "image JSON %s", in)
	}
}

func TestMoidLess(t *testing.T) {
	assert.True(t, moidLess("vm-99", "vm-100"), "numeric, not lexicographic, order")
	assert.False(t, moidLess("vm-100", "vm-99"))
	assert.True(t, moidLess("vm-5", "vm-6"))
	assert.False(t, moidLess("vm-6", "vm-6"))
}

// obj is an artifactObject for decision tests.
func obj(moid string, template bool, stamp *imageartifact.Stamp) artifactObject {
	return artifactObject{ref: vmRef(moid), template: template, poweredOff: true, stamp: stamp}
}

func TestDecideArtifact(t *testing.T) {
	now := time.Now()
	bound := 2 * time.Hour
	fresh := now.Add(-time.Minute)
	stale := now.Add(-3 * time.Hour)
	req := testIdentity(testDigestA)
	other := &imageartifact.Stamp{}
	*other = *stampAt(testDigestA, fresh)
	other.Image.UID = testOtherUID
	otherDigest := stampAt(testDigestB, fresh)

	withTask := obj("vm-10", false, stampAt(testDigestA, stale))
	withTask.activeTask = true
	held := obj("vm-10", false, stampAt(testDigestA, stale))
	held.destroyDisabled = true
	poweredOn := obj("vm-10", false, stampAt(testDigestA, stale))
	poweredOn.poweredOff = false
	untrusted := obj("vm-10", true, nil)
	untrusted.stampErr = fmt.Errorf("repeated key")

	single := map[string]struct {
		o    artifactObject
		want imageartifact.Outcome
	}{
		"matching template":                      {obj("vm-10", true, stampAt(testDigestA, stale)), imageartifact.OutcomeReuse},
		"template of another image":              {obj("vm-10", true, other), imageartifact.OutcomeConflict},
		"template of another source":             {obj("vm-10", true, otherDigest), imageartifact.OutcomeConflict},
		"unstamped template":                     {obj("vm-10", true, nil), imageartifact.OutcomeConflict},
		"untrusted stamp":                        {untrusted, imageartifact.OutcomeConflict},
		"unstamped VM":                           {obj("vm-10", false, nil), imageartifact.OutcomeConflict},
		"VM of another image":                    {obj("vm-10", false, other), imageartifact.OutcomeConflict},
		"unfinished, fresh":                      {obj("vm-10", false, stampAt(testDigestA, fresh)), imageartifact.OutcomeInProgress},
		"unfinished, prepared in the future":     {obj("vm-10", false, stampAt(testDigestA, now.Add(time.Hour))), imageartifact.OutcomeInProgress},
		"unfinished, stale but a task runs":      {withTask, imageartifact.OutcomeInProgress},
		"unfinished, stale but Destroy disabled": {held, imageartifact.OutcomeInProgress},
		"unfinished, stale, powered on":          {poweredOn, imageartifact.OutcomeConflict},
		"unfinished, stale, idle":                {obj("vm-10", false, stampAt(testDigestA, stale)), imageartifact.OutcomeAbandoned},
	}
	for name, tc := range single {
		t.Run(name, func(t *testing.T) {
			d := decideArtifact([]artifactObject{tc.o}, req, now, bound)
			assert.Equal(t, tc.want, d.outcome)
			require.NotNil(t, d.target)
			assert.Equal(t, tc.o.ref, d.target.ref)
			assert.Empty(t, d.cleanup)
		})
	}

	t.Run("nothing at the name", func(t *testing.T) {
		d := decideArtifact(nil, req, now, bound)
		assert.Equal(t, imageartifact.OutcomeImport, d.outcome)
		assert.Nil(t, d.target)
	})

	mine := obj("vm-99", true, stampAt(testDigestA, stale))
	converging := obj("vm-100", false, stampAt(testDigestA, fresh))
	abandoned := obj("vm-100", false, stampAt(testDigestA, stale))
	foreign := obj("vm-100", true, other)
	secondTemplate := obj("vm-100", true, stampAt(testDigestA, stale))

	t.Run("several: a converging copy is in progress", func(t *testing.T) {
		d := decideArtifact([]artifactObject{converging, mine}, req, now, bound)
		assert.Equal(t, imageartifact.OutcomeInProgress, d.outcome)
		assert.Equal(t, converging.ref, d.target.ref)
	})
	t.Run("several: an abandoned copy is cleaned up first", func(t *testing.T) {
		d := decideArtifact([]artifactObject{abandoned, mine}, req, now, bound)
		require.Len(t, d.cleanup, 1)
		assert.Equal(t, abandoned.ref, d.cleanup[0].ref)
	})
	t.Run("several: a foreign object makes the name ambiguous", func(t *testing.T) {
		d := decideArtifact([]artifactObject{mine, foreign}, req, now, bound)
		assert.Equal(t, imageartifact.OutcomeConflict, d.outcome)
		assert.Equal(t, foreign.ref, d.target.ref)
	})
	t.Run("several: a second complete template is a conflict", func(t *testing.T) {
		d := decideArtifact([]artifactObject{mine, secondTemplate}, req, now, bound)
		assert.Equal(t, imageartifact.OutcomeConflict, d.outcome)
	})
	t.Run("several: the lowest MOID decides first", func(t *testing.T) {
		lowForeign := obj("vm-98", true, other)
		d := decideArtifact([]artifactObject{mine, lowForeign}, req, now, bound)
		assert.Equal(t, imageartifact.OutcomeConflict, d.outcome)
		assert.Equal(t, lowForeign.ref, d.target.ref)
	})
}

func TestIsDuplicateNameFault(t *testing.T) {
	dup := &types.DuplicateName{Name: "x"}
	soapFault := &soap.Fault{Code: "ServerFaultCode", String: "The name 'x' already exists."}
	soapFault.Detail.Fault = dup

	assert.True(t, isDuplicateNameFault(soap.WrapSoapFault(soapFault)), "synchronous ImportVApp fault")
	assert.True(t, isDuplicateNameFault(fmt.Errorf("ImportVApp: %w", soap.WrapSoapFault(soapFault))), "wrapped")
	assert.True(t, isDuplicateNameFault(&task.Error{LocalizedMethodFault: &types.LocalizedMethodFault{Fault: dup}}),
		"the HttpNfcLease error path")
	assert.False(t, isDuplicateNameFault(nil))
	assert.False(t, isDuplicateNameFault(fmt.Errorf("boom")))
	assert.False(t, isDuplicateNameFault(soap.WrapVimFault(&types.NotAuthenticated{})))
}

func TestIsOVFContentFault(t *testing.T) {
	assert.True(t, isOVFContentFault(soap.WrapVimFault(&types.InvalidArgument{InvalidProperty: "ovfDescriptor"})))
	assert.True(t, isOVFContentFault(fmt.Errorf("x: %w", soap.WrapVimFault(&types.OvfXmlFormat{}))), "an OVF fault")
	assert.False(t, isOVFContentFault(soap.WrapVimFault(&types.InvalidArgument{InvalidProperty: "pool"})),
		"an InvalidArgument about anything but the descriptor is not a content problem")
	assert.False(t, isOVFContentFault(soap.WrapVimFault(&types.NotAuthenticated{})))
	assert.False(t, isOVFContentFault(fmt.Errorf("connection reset")))
	assert.False(t, isOVFContentFault(nil))
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://images.example.com/u.ova":                        "https://images.example.com/u.ova",
		"https://user:s3cret@images.example.com/u.ova":            "https://images.example.com/u.ova",
		"https://images.example.com/u.ova?X-Amz-Signature=abc123": "https://images.example.com/u.ova?<redacted>",
		"https://images.example.com/u.ova#frag":                   "https://images.example.com/u.ova?<redacted>",
		"://bad":                                                  "<unparseable URL>",
	} {
		assert.Equal(t, want, redactURL(in), "input %q", in)
	}
}

func TestURLPathExt(t *testing.T) {
	assert.Equal(t, ".ovf", urlPathExt("https://h/x.OVF?sig=abc.ova"))
	assert.Equal(t, ".ova", urlPathExt("https://h/x.ova"))
	assert.Equal(t, "", urlPathExt("https://h/download"))
}

func TestIsPermanentHTTPStatus(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		assert.True(t, isPermanentHTTPStatus(code), "%d", code)
	}
	for _, code := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusMovedPermanently} {
		assert.False(t, isPermanentHTTPStatus(code), "%d", code)
	}
}

func TestIdentitySourceError(t *testing.T) {
	ok := vsphereImageSource{OVAURL: "https://images.example.com/u.ova"}
	assert.NoError(t, identitySourceError(ok))
	for name, src := range map[string]vsphereImageSource{
		"no ovaURL":                    {},
		"templateName only":            {TemplateName: "golden"},
		"contentLibrary only":          {ContentLibrary: &vsphereContentLibraryRef{Library: "l", Item: "i"}},
		"ovaURL and templateName":      {OVAURL: ok.OVAURL, TemplateName: "golden"},
		"ovaURL and contentLibrary":    {OVAURL: ok.OVAURL, ContentLibrary: &vsphereContentLibraryRef{Library: "l", Item: "i"}},
		"not http(s)":                  {OVAURL: "ftp://images.example.com/u.ova"},
		"no host":                      {OVAURL: "https:///u.ova"},
		"credentials are never echoed": {OVAURL: "file://user:s3cret@/etc/passwd"},
		"unparseable":                  {OVAURL: "://bad"},
	} {
		t.Run(name, func(t *testing.T) {
			err := identitySourceError(src)
			requireCode(t, err, codes.InvalidArgument)
			assert.NotContains(t, err.Error(), "s3cret")
		})
	}
}
