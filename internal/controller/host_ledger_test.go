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

package controller

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// ledgerTB is what the ledger test helpers need of a test (testing.TB or
// GinkgoT()).
type ledgerTB interface {
	require.TestingT
	Helper()
}

// randomLedgerKey returns 32 random bytes.
func randomLedgerKey(t ledgerTB) []byte {
	t.Helper()
	k := make([]byte, hostLedgerKeyLen)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// newTestLedgerKeys returns ledger keys with random tag and identity keys
// and the raw current key.
func newTestLedgerKeys(t ledgerTB) *HostLedgerKeys {
	t.Helper()
	k, err := NewHostLedgerKeys(randomLedgerKey(t), nil, randomLedgerKey(t))
	require.NoError(t, err)
	return k
}

// sampleLedgerRecord returns a valid, untagged record.
func sampleLedgerRecord() *HostLedgerRecord {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	return &HostLedgerRecord{
		StoreID: [16]byte{0xab, 0xcd, 0xef, 0x01, 5}, HostIdentityMAC: [32]byte{7}, HostUID: "uid-1", HostKey: "ns/h", ProviderUID: "puid",
		Position: HostLedgerPosition{Seq: 2050, Head: [32]byte{1}}, Checkpoint: HostLedgerPosition{Seq: 2048, Head: [32]byte{2}},
		AttestationID: [16]byte{9}, AttestationState: infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested,
		PendingSessions:      []HostLedgerPendingSession{{ID: [16]byte{3}, BegunAt: at}},
		SessionHistory:       []HostLedgerFencedSession{{ID: [16]byte{4}, FencedAt: 1000}},
		Generation:           1764150000,
		GenerationStartedAt:  at,
		GenerationEnrolments: [][]byte{bytes.Repeat([]byte{0xa1}, 16)},
		WriteCounter:         3, MinManagerVersion: hostLedgerMinManagerVersion, UpdatedAt: at,
	}
}

func TestHostLedgerKeys(t *testing.T) {
	cur, prev, ident := randomLedgerKey(t), randomLedgerKey(t), randomLedgerKey(t)
	k, err := NewHostLedgerKeys(cur, prev, ident)
	require.NoError(t, err)
	assert.Len(t, k.CurrentKeyID(), 16)
	assert.Equal(t, hostLedgerKeyID(cur), k.CurrentKeyID(), "the id names the key")
	assert.Equal(t, hostLedgerKeyID(prev), k.PreviousKeyID())

	// No key material in any rendering.
	for _, s := range []string{k.String(), fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k)} {
		for _, raw := range [][]byte{cur, prev, ident} {
			assert.NotContains(t, s, hex.EncodeToString(raw))
		}
	}

	for name, keys := range map[string][3][]byte{
		"short current":           {cur[:31], nil, ident},
		"zero current":            {make([]byte, 32), nil, ident},
		"no identity":             {cur, nil, nil},
		"identity equals current": {cur, nil, cur},
		"previous equals current": {cur, cur, ident},
		"identity equals prev":    {cur, prev, prev},
		"short previous":          {cur, prev[:5], ident},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewHostLedgerKeys(keys[0], keys[1], keys[2])
			assert.ErrorIs(t, err, errHostLedgerKeys)
		})
	}

	// Tags verify under the key their id names, current or previous; an
	// unknown id never verifies; contexts separate.
	id, sum := k.tag(hostLedgerTagContext, []byte("m"))
	assert.True(t, k.verifyTag(id, hostLedgerTagContext, []byte("m"), sum[:]))
	assert.False(t, k.verifyTag(id, hostLedgerIndexTagContext, []byte("m"), sum[:]), "a tag never verifies in another context")
	assert.False(t, k.verifyTag(id, hostLedgerTagContext, []byte("n"), sum[:]))
	assert.False(t, k.verifyTag("0000000000000000", hostLedgerTagContext, []byte("m"), sum[:]))
	assert.False(t, k.verifyTag("", hostLedgerTagContext, []byte("m"), sum[:]))
	rotated, err := NewHostLedgerKeys(randomLedgerKey(t), cur, ident)
	require.NoError(t, err)
	assert.True(t, rotated.verifyTag(id, hostLedgerTagContext, []byte("m"), sum[:]), "the previous key still verifies")
	alone, err := NewHostLedgerKeys(randomLedgerKey(t), nil, ident)
	require.NoError(t, err)
	assert.False(t, alone.verifyTag(id, hostLedgerTagContext, []byte("m"), sum[:]), "a dropped key no longer verifies")

	// The identity MAC is keyed: another identity key or install id gives
	// another value.
	install := bytes.Repeat([]byte{0x42}, 16)
	a := k.IdentityMAC(install, "serial-1")
	assert.Equal(t, a, rotated.IdentityMAC(install, "serial-1"), "rotating the tag key keeps identity MACs")
	assert.NotEqual(t, a, k.IdentityMAC(install, "serial-2"))
	assert.NotEqual(t, a, k.IdentityMAC(bytes.Repeat([]byte{0x43}, 16), "serial-1"))
	other, err := NewHostLedgerKeys(cur, nil, randomLedgerKey(t))
	require.NoError(t, err)
	assert.NotEqual(t, a, other.IdentityMAC(install, "serial-1"))
}

func TestReadHostLedgerKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := randomLedgerKey(t)
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	write("ok", hex.EncodeToString(key)+"\n")
	got, err := readHostLedgerKeyFile(dir, "ok", false)
	require.NoError(t, err)
	assert.Equal(t, key, got)
	write("upper", strings.ToUpper(hex.EncodeToString(key)))
	write("short", hex.EncodeToString(key[:31]))
	write("nothex", strings.Repeat("zz", 32))
	for _, name := range []string{"upper", "short", "nothex", "absent"} {
		_, err := readHostLedgerKeyFile(dir, name, false)
		assert.ErrorIs(t, err, errHostLedgerKeys, name)
		assert.NotContains(t, err.Error(), hex.EncodeToString(key), "no key material in errors")
	}
	got, err = readHostLedgerKeyFile(dir, "absent", true)
	require.NoError(t, err)
	assert.Nil(t, got, "a missing optional file is no key")
}

func TestHostLedgerRecord_RoundTripAndTag(t *testing.T) {
	keys := newTestLedgerKeys(t)
	r := sampleLedgerRecord()
	r.sign(keys)
	data, err := r.encode()
	require.NoError(t, err)
	got, err := decodeHostLedger(data)
	require.NoError(t, err)
	assert.True(t, got.verifyTag(keys))
	again, err := got.encode()
	require.NoError(t, err)
	assert.JSONEq(t, string(data), string(again))
	assert.Equal(t, "abcdef01", got.StorePrefix())

	// Every tagged field is covered; updatedAt is not.
	for name, edit := range map[string]func(*HostLedgerRecord){
		"seq":         func(r *HostLedgerRecord) { r.Position.Seq++ },
		"head":        func(r *HostLedgerRecord) { r.Position.Head[0] ^= 1 },
		"checkpoint":  func(r *HostLedgerRecord) { r.Checkpoint.Head[0] ^= 1 },
		"store":       func(r *HostLedgerRecord) { r.StoreID[15] ^= 1 },
		"identity":    func(r *HostLedgerRecord) { r.HostIdentityMAC[0] ^= 1 },
		"host uid":    func(r *HostLedgerRecord) { r.HostUID = "uid-2" },
		"host key":    func(r *HostLedgerRecord) { r.HostKey = "ns/other" },
		"provider":    func(r *HostLedgerRecord) { r.ProviderUID = "other" },
		"attestation": func(r *HostLedgerRecord) { r.AttestationID[0] ^= 1 },
		"state": func(r *HostLedgerRecord) {
			r.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
		},
		"pending": func(r *HostLedgerRecord) { r.PendingSessions = nil },
		"pending time": func(r *HostLedgerRecord) {
			r.PendingSessions[0].BegunAt = r.PendingSessions[0].BegunAt.Add(time.Second)
		},
		"history":              func(r *HostLedgerRecord) { r.SessionHistory[0].FencedAt++ },
		"generation":           func(r *HostLedgerRecord) { r.Generation++ },
		"generation started":   func(r *HostLedgerRecord) { r.GenerationStartedAt = r.GenerationStartedAt.Add(time.Hour) },
		"generation enrolment": func(r *HostLedgerRecord) { r.GenerationEnrolments[0][0] ^= 1 },
		"write counter":        func(r *HostLedgerRecord) { r.WriteCounter-- },
		"floor":                func(r *HostLedgerRecord) { r.MinManagerVersion = "0.0.1" },
		"archived": func(r *HostLedgerRecord) {
			r.Archived, r.SuccessorStoreID, r.ArchivedAt = true, [16]byte{1}, r.UpdatedAt
		},
		"unknown member": func(r *HostLedgerRecord) { r.unknown = unknownMembers{"future": json.RawMessage(`1`)} },
	} {
		t.Run(name, func(t *testing.T) {
			c := got.clone()
			edit(c)
			assert.False(t, c.verifyTag(keys), "an edit of %s breaks the tag", name)
		})
	}
	c := got.clone()
	c.UpdatedAt = c.UpdatedAt.Add(time.Hour)
	assert.True(t, c.verifyTag(keys), "updatedAt is not tagged")

	// A tag under another key or an unknown key id fails.
	c = got.clone()
	c.TagKeyID = "0123456789abcdef"
	assert.False(t, c.verifyTag(keys))
	assert.False(t, got.verifyTag(newTestLedgerKeys(t)))
}

func TestHostLedgerRecord_UnknownMembersKeptAndTagged(t *testing.T) {
	keys := newTestLedgerKeys(t)
	r := sampleLedgerRecord()
	r.unknown = unknownMembers{"futureTop": json.RawMessage(`{"a": [1, 2]}`)}
	r.PendingSessions[0].unknown = unknownMembers{"futureNested": json.RawMessage(`"x"`)}
	r.SessionHistory[0].unknown = unknownMembers{"futureFenced": json.RawMessage(`true`)}
	r.sign(keys)
	data, err := r.encode()
	require.NoError(t, err)
	got, err := decodeHostLedger(data)
	require.NoError(t, err)
	require.True(t, got.verifyTag(keys), "a newer manager's members are covered and verify")
	assert.Contains(t, got.unknown, "futureTop")
	assert.Contains(t, got.PendingSessions[0].unknown, "futureNested")
	assert.Contains(t, got.SessionHistory[0].unknown, "futureFenced")
	// Rewritten by this manager: kept, still covered.
	got.Position.Seq++
	got.sign(keys)
	out, err := got.encode()
	require.NoError(t, err)
	for _, m := range []string{"futureTop", "futureNested", "futureFenced"} {
		assert.Contains(t, string(out), m)
	}
	back, err := decodeHostLedger(out)
	require.NoError(t, err)
	assert.True(t, back.verifyTag(keys))
	// An attacker adding a member breaks the tag.
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	doc["injected"] = "x"
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	inj, err := decodeHostLedger(b)
	require.NoError(t, err)
	assert.False(t, inj.verifyTag(keys))
}

func TestDecodeHostLedger_RefusesMalformed(t *testing.T) {
	keys := newTestLedgerKeys(t)
	base := sampleLedgerRecord()
	base.sign(keys)
	data, err := base.encode()
	require.NoError(t, err)
	edit := func(f func(map[string]any)) []byte {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(data, &doc))
		f(doc)
		b, err := json.Marshal(doc)
		require.NoError(t, err)
		return b
	}
	for name, doc := range map[string][]byte{
		"empty":                  nil,
		"not json":               []byte("{"),
		"too large":              append(append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), maxLedgerSecretBytes)...), '"', '}'),
		"short store":            edit(func(d map[string]any) { d["storeID"] = "abc" }),
		"upper hex":              edit(func(d map[string]any) { d["head"] = strings.Repeat("A", 64) }),
		"seq zero":               edit(func(d map[string]any) { d["seq"] = 0 }),
		"checkpoint not 1024":    edit(func(d map[string]any) { d["checkpointSeq"] = 1000 }),
		"checkpoint above seq":   edit(func(d map[string]any) { d["checkpointSeq"] = 4096 }),
		"bad state":              edit(func(d map[string]any) { d["attestationState"] = "Maybe" }),
		"attested without id":    edit(func(d map[string]any) { delete(d, "attestationID") }),
		"five pending":           edit(func(d map[string]any) { d["pendingSessions"] = fivePending() }),
		"duplicate pending":      edit(func(d map[string]any) { d["pendingSessions"] = duplicatePending() }),
		"no generation":          edit(func(d map[string]any) { d["ledgerGeneration"] = 0 }),
		"no enrolments":          edit(func(d map[string]any) { d["generationEnrolments"] = []string{} }),
		"local time":             edit(func(d map[string]any) { d["generationStartedAt"] = "2026-11-26T11:00:00+01:00" }),
		"successor not archived": edit(func(d map[string]any) { d["successorStoreID"] = strings.Repeat("0", 32) }),
		"bad key id":             edit(func(d map[string]any) { d["tagKeyID"] = "xyz" }),
		"no host":                edit(func(d map[string]any) { d["hostUID"] = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeHostLedger(doc)
			assert.ErrorIs(t, err, errLedgerMalformed)
		})
	}
}

func fivePending() []map[string]any {
	var out []map[string]any
	for i := range 5 {
		out = append(out, map[string]any{"id": fmt.Sprintf("%032x", i+1), "w1At": "2026-11-26T10:00:00Z"})
	}
	return out
}

func duplicatePending() []map[string]any {
	p := map[string]any{"id": strings.Repeat("1", 32), "w1At": "2026-11-26T10:00:00Z"}
	return []map[string]any{p, p}
}

func TestHostLedgerIndex_TagBindsHost(t *testing.T) {
	keys := newTestLedgerKeys(t)
	x := &HostLedgerIndex{StoreIDs: [][16]byte{{1}, {2}}, CounterSeen: 7}
	x.sign(keys, "uid-a")
	assert.True(t, x.verifyTag(keys, "uid-a"))
	assert.False(t, x.verifyTag(keys, "uid-b"), "an index copied from another Host's pins Secret does not verify")
	c := x.clone()
	c.CounterSeen = 6
	assert.False(t, c.verifyTag(keys, "uid-a"), "a lowered counter breaks the tag")
	c = x.clone()
	c.StoreIDs = c.StoreIDs[1:]
	assert.False(t, c.verifyTag(keys, "uid-a"), "a removed store breaks the tag")
	assert.True(t, x.Names([16]byte{2}))
	assert.False(t, (*HostLedgerIndex)(nil).Names([16]byte{2}))
}

// The ledger index lives in store.json: every store-state write of the
// manager keeps it, a malformed one is kept as it is (and refused by the
// ledger as tampered), and unknown members around it survive.
func TestStoreState_KeepsLedgerIndex(t *testing.T) {
	keys := newTestLedgerKeys(t)
	x := &HostLedgerIndex{StoreIDs: [][16]byte{{1}}, CounterSeen: 4}
	x.sign(keys, "uid-a")
	st := HostStoreState{Current: hostidentity.StoreRecord{StoreID: [16]byte{1}, EnrolmentID: bytes.Repeat([]byte{1}, 16), Seq: 3,
		Head: [32]byte{3}}, LedgerIndex: x}
	data, err := encodeStoreState(st)
	require.NoError(t, err)
	got, err := decodeStoreState(data)
	require.NoError(t, err)
	require.NotNil(t, got.LedgerIndex)
	assert.True(t, got.LedgerIndex.verifyTag(keys, "uid-a"))
	got.Current.Seq = 9 // a position write
	again, err := encodeStoreState(got)
	require.NoError(t, err)
	back, err := decodeStoreState(again)
	require.NoError(t, err)
	assert.True(t, back.LedgerIndex.verifyTag(keys, "uid-a"), "a position write keeps the index")

	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &doc))
	doc["ledgerIndex"] = json.RawMessage(`{"storeIDs":["zz"],"counterSeen":1,"tagKeyID":"x","tag":"y"}`)
	doc["futureTop"] = json.RawMessage(`[1]`)
	bad, err := json.Marshal(doc)
	require.NoError(t, err)
	st2, err := decodeStoreState(bad)
	require.NoError(t, err, "a malformed index does not make the positions unreadable")
	assert.Nil(t, st2.LedgerIndex)
	assert.NotEmpty(t, st2.ledgerIndexRaw)
	out, err := encodeStoreState(st2)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"storeIDs":["zz"]`, "kept as it is")
	assert.Contains(t, string(out), "futureTop")
}

func TestLedgerFloorChanged(t *testing.T) {
	ledger := func(floor, seq string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "l", Labels: map[string]string{LabelHostLedger: "v1",
			k8sLabelComponent: hostLedgerComponent}},
			Data: map[string][]byte{hostLedgerDataKey: []byte(`{"minManagerVersion":"` + floor + `","seq":` + seq + `}`)}}
	}
	upd := func(o, n *corev1.Secret) bool {
		return ledgerFloorChanged.Update(event.UpdateEvent{ObjectOld: o, ObjectNew: n})
	}
	assert.False(t, upd(ledger("0.4.2-0", "1"), ledger("0.4.2-0", "2")), "a ledger write that keeps the floor is not an enrolment event")
	assert.True(t, upd(ledger("0.4.2-0", "1"), ledger("0.6.0", "2")), "a floor change is")
	pins := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{k8sLabelComponent: hostEnrolmentComponent}}}
	assert.True(t, upd(pins, pins.DeepCopy()), "pins Secrets always pass")
	moved := ledger("0.4.2-0", "2")
	moved.Annotations = map[string]string{"enrolment.virtrigaud.io/host": "ns/other"}
	assert.True(t, upd(ledger("0.4.2-0", "1"), moved), "a ledger that serves another Host passes")
	assert.True(t, ledgerFloorChanged.Create(event.CreateEvent{Object: ledger("0.4.2-0", "1")}))
}
