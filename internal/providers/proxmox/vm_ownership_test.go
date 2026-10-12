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

package proxmox

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	testOwnerUID   = "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"
	testOtherUID   = "0a1b2c3d-4e5f-4061-8273-94a5b6c7d8e9"
	testNamespace  = "team-a"
	testOwnerName  = "web"
	testUserNotes  = "Ubuntu 24.04 golden image\nbuilt by ops"
	testStampPhase = ownerStampPhaseReady
)

func testOwner() contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: testOwnerName}
}

func testStamp(uid, phase string) ownerStamp {
	return ownerStamp{owner: contracts.ObjectIdentity{UID: uid, Namespace: testNamespace, Name: testOwnerName}, phase: phase}
}

func TestOwnerStamp_RoundTrip(t *testing.T) {
	for _, phase := range []string{ownerStampPhaseCreating, ownerStampPhaseReady} {
		s := testStamp(testOwnerUID, phase)
		line := s.line()
		assert.True(t, strings.HasPrefix(line, "virtrigaud.owner v1 uid="+testOwnerUID+" namespace=team-a name=web phase="+phase+" sum="), line)
		got, err := parseOwnerStampLine(line)
		require.NoError(t, err)
		assert.Equal(t, s, got)

		read := readOwnerStamp(testUserNotes + "\n" + line + "\n")
		require.Empty(t, read.state)
		require.NotNil(t, read.stamp)
		assert.Equal(t, s, *read.stamp)
		assert.Equal(t, []string{testOwnerUID}, read.uids)
	}
}

func TestOwnerStamp_CRLFAndIndentationAreTolerated(t *testing.T) {
	line := testStamp(testOwnerUID, testStampPhase).line()
	read := readOwnerStamp("notes\r\n  " + line + "\r\n")
	require.Empty(t, read.state)
	require.NotNil(t, read.stamp)
	assert.Equal(t, testOwnerUID, read.stamp.owner.UID)
}

// TestOwnerStamp_TamperedLinesAreUnreadable: any edit of a stamp line, or a
// line that mentions the marker without being a canonical stamp, is
// unreadable — never a different owner.
func TestOwnerStamp_TamperedLinesAreUnreadable(t *testing.T) {
	line := testStamp(testOwnerUID, testStampPhase).line()
	cases := map[string]string{
		"uid edited, old checksum":     strings.Replace(line, testOwnerUID, testOtherUID, 1),
		"namespace edited":             strings.Replace(line, "namespace=team-a", "namespace=team-b", 1),
		"phase edited":                 strings.Replace(line, "phase=ready", "phase=creating", 1),
		"checksum edited":              line[:len(line)-1] + "0",
		"double space":                 strings.Replace(line, " uid=", "  uid=", 1),
		"tab separator":                strings.Replace(line, " uid=", "\tuid=", 1),
		"upper-case marker":            strings.Replace(line, "virtrigaud.owner", "VirtRigaud.Owner", 1),
		"unknown version":              strings.Replace(line, " v1 ", " v2 ", 1),
		"missing field":                strings.Replace(line, " phase=ready", "", 1),
		"extra field":                  line + " extra=1",
		"fields reordered":             strings.Replace(line, "namespace=team-a name=web", "name=web namespace=team-a", 1),
		"markdown bullet":              "- " + line,
		"prose mentioning the marker":  "do not edit the virtrigaud.owner line",
		"marker with colon":            "virtrigaud.owner: uid=" + testOwnerUID,
		"unknown phase, valid sum":     reSum("virtrigaud.owner v1 uid=" + testOwnerUID + " namespace=team-a name=web phase=done"),
		"space in a value, valid sum":  reSum("virtrigaud.owner v1 uid=" + testOwnerUID + " namespace=team a name=web phase=ready"),
		"equals in a value, valid sum": reSum("virtrigaud.owner v1 uid=" + testOwnerUID + " namespace=a=b name=web phase=ready"),
		"empty uid, valid sum":         reSum("virtrigaud.owner v1 uid= namespace=team-a name=web phase=ready"),
	}
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			read := readOwnerStamp("notes\n" + desc)
			assert.Equal(t, contracts.OwnerStampUnreadable, read.state)
			assert.Nil(t, read.stamp)
		})
	}
}

// reSum appends a valid checksum to a stamp body, so a test reaches the
// field validation behind it.
func reSum(body string) string {
	return body + " sum=" + ownerStampSum(body)
}

func TestOwnerStamp_MultipleAndRepeated(t *testing.T) {
	mine := testStamp(testOwnerUID, testStampPhase).line()
	other := testStamp(testOtherUID, testStampPhase).line()

	read := readOwnerStamp(mine + "\n" + other)
	assert.Equal(t, contracts.OwnerStampMultiple, read.state)
	assert.Nil(t, read.stamp)
	assert.Equal(t, []string{testOwnerUID, testOtherUID}, read.uids)

	read = readOwnerStamp(mine + "\n" + mine)
	assert.Equal(t, contracts.OwnerStampUnreadable, read.state, "VirtRigaud writes one stamp line")
	assert.Equal(t, []string{testOwnerUID}, read.uids)

	read = readOwnerStamp(mine + "\nvirtrigaud.owner garbage")
	assert.Equal(t, contracts.OwnerStampUnreadable, read.state, "unreadable wins over a readable stamp")
	assert.Equal(t, []string{testOwnerUID}, read.uids)
}

func TestOwnerStamp_NoStamp(t *testing.T) {
	for _, desc := range []string{"", testUserNotes, "virtrigaud managed\nowner: ops"} {
		read := readOwnerStamp(desc)
		assert.Empty(t, read.state)
		assert.Nil(t, read.stamp)
		assert.Empty(t, read.uids)
	}
}

func TestComposeDescription(t *testing.T) {
	mine := testStamp(testOwnerUID, testStampPhase)
	foreign := testStamp(testOtherUID, testStampPhase).line()

	// The inherited notes are kept, every stamp line is replaced.
	got := composeDescription(testUserNotes+"\n"+foreign+"\nvirtrigaud.owner junk\n\n", &mine)
	assert.Equal(t, testUserNotes+"\n"+mine.line(), got)
	read := readOwnerStamp(got)
	require.NotNil(t, read.stamp)
	assert.Equal(t, mine, *read.stamp)

	// No stamp: the stamp lines are only removed.
	assert.Equal(t, testUserNotes, composeDescription(testUserNotes+"\n"+foreign, nil))
	assert.Empty(t, composeDescription(foreign, nil))
	assert.Equal(t, mine.line(), composeDescription("", &mine))

	// L6: too long for PVE with the stamp: the notes are truncated (at a
	// character boundary, marked), never dropped, and the stamp is kept.
	long := strings.Repeat("é", pveDescriptionMaxLen) // 2 bytes each
	got = composeDescription(long, &mine)
	assert.LessOrEqual(t, len(got), pveDescriptionMaxLen)
	assert.True(t, utf8.ValidString(got))
	assert.True(t, strings.HasPrefix(got, "éé"), "the start of the notes is kept")
	assert.Contains(t, got, descriptionTruncatedNote)
	assert.True(t, strings.HasSuffix(got, "\n"+mine.line()))
	read = readOwnerStamp(got)
	require.NotNil(t, read.stamp)
	assert.Equal(t, mine, *read.stamp)
	assert.True(t, descriptionTruncated(long, &mine))
	assert.False(t, descriptionTruncated(testUserNotes, &mine))
	assert.False(t, descriptionTruncated(long, nil), "without a stamp nothing is added, so nothing is cut")
}

func TestValidateOwnerIdentity(t *testing.T) {
	ok := testOwner()
	require.NoError(t, validateOwnerIdentity(ok))
	require.NoError(t, validateOwnerIdentity(contracts.ObjectIdentity{UID: "uid-1", Namespace: "ns", Name: "a.b-c_d"}))

	bad := map[string]contracts.ObjectIdentity{
		"empty uid":        {UID: "", Namespace: "ns", Name: "n"},
		"empty namespace":  {UID: "u", Namespace: "", Name: "n"},
		"empty name":       {UID: "u", Namespace: "ns", Name: ""},
		"space":            {UID: "u", Namespace: "a b", Name: "n"},
		"equals":           {UID: "u=x", Namespace: "ns", Name: "n"},
		"newline":          {UID: "u", Namespace: "ns", Name: "n\nvirtrigaud.owner"},
		"non-ascii":        {UID: "u", Namespace: "ns", Name: "wéb"},
		"leading dash":     {UID: "u", Namespace: "-ns", Name: "n"},
		"semicolon (tags)": {UID: "u", Namespace: "ns", Name: "a;b"},
		"too long":         {UID: "u", Namespace: "ns", Name: strings.Repeat("a", ownerIdentityFieldMaxLen+1)},
	}
	for name, o := range bad {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, validateOwnerIdentity(o))
		})
	}
}

func TestOwnerFromCreateRequest(t *testing.T) {
	o, err := ownerFromCreateRequest(&providerv1.CreateRequest{})
	require.NoError(t, err)
	assert.True(t, o.IsZero(), "no owner: an older manager")

	o, err = ownerFromCreateRequest(&providerv1.CreateRequest{Owner: &providerv1.ObjectIdentity{Namespace: "ns", Name: "n"}})
	require.NoError(t, err)
	assert.True(t, o.IsZero(), "an owner without a UID proves nothing")

	_, err = ownerFromCreateRequest(&providerv1.CreateRequest{Owner: &providerv1.ObjectIdentity{Uid: testOwnerUID, Namespace: "a b", Name: "n"}})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	o, err = ownerFromCreateRequest(&providerv1.CreateRequest{Owner: &providerv1.ObjectIdentity{Uid: testOwnerUID, Namespace: testNamespace, Name: testOwnerName}})
	require.NoError(t, err)
	assert.Equal(t, testOwner(), o)
}

func TestOwnerRaw(t *testing.T) {
	raw := map[string]string{}
	ownerRaw(raw, readOwnerStamp(testStamp(testOwnerUID, testStampPhase).line()))
	assert.Equal(t, map[string]string{contracts.VMInfoOwnerUIDKey: testOwnerUID}, raw)

	raw = map[string]string{}
	ownerRaw(raw, readOwnerStamp(testStamp(testOwnerUID, testStampPhase).line()+"\n"+testStamp(testOtherUID, testStampPhase).line()))
	assert.Equal(t, testOwnerUID+","+testOtherUID, raw[contracts.VMInfoOwnerUIDKey])
	assert.Equal(t, contracts.OwnerStampMultiple, raw[contracts.VMInfoOwnerStampStateKey])

	raw = map[string]string{}
	ownerRaw(raw, readOwnerStamp("virtrigaud.owner broken"))
	assert.Equal(t, map[string]string{contracts.VMInfoOwnerStampStateKey: contracts.OwnerStampUnreadable}, raw)

	raw = map[string]string{}
	ownerRaw(raw, readOwnerStamp(testUserNotes))
	assert.Empty(t, raw)
}
