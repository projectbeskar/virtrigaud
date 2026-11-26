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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// pinBuildVersion sets this manager's version for one test.
func pinBuildVersion(t *testing.T, v string) {
	t.Helper()
	prev := buildVersion
	buildVersion = func() string { return v }
	t.Cleanup(func() { buildVersion = prev })
}

// previousEnrolmentJSON is an enrolment state document exactly as the
// encoder before unknown-member preservation wrote it (struct field order,
// compact), with every nested record type present.
const previousEnrolmentJSON = `{"hostUID":"uid-1","identity":"serial-1",` +
	`"pending":{"enrolmentID":"aa","providerUID":"p","installID":"bb","startEpoch":3},` +
	`"authorised":{"request":"ns/cr","requestUID":"u","issuer":"certmanager","record":{"enrolmentID":"aa","providerUID":"p","installID":"bb"},"keyPin":"k","requester":"alice"},` +
	`"current":{"record":{"enrolmentID":"cc","providerUID":"p","installID":"bb","startEpoch":2},"keyPins":["k1"],"notAfter":"2030-01-01T00:00:00Z","notBefore":"2029-01-01T00:00:00Z"},` +
	`"retiredEnrolmentIDs":["dd"],"predecessorEnrolmentIDs":["ee"],"predecessorSince":{"ee":"2029-06-01T00:00:00Z"},` +
	`"lastIssuedEpoch":3,"previousKeyPins":["k0"],"rekey":{"value":"v","until":"2029-07-01T00:00:00Z"},"lastRekeyRequest":"w",` +
	`"lastDenial":{"request":"ns/x","reason":"r"},"lastRejection":{"request":"ns/y","reason":"r","message":"m"},` +
	`"renewal":{"request":"cmVx","keyPin":"k2","signerPin":"s","since":"2029-08-01T00:00:00Z","approved":true},` +
	`"renewalAttempts":[{"keyPin":"k3","failures":1,"nextTry":"2029-09-01T00:00:00Z"}]}`

// A document the previous encoder wrote decodes and encodes back to the same
// bytes: without unknown members the output format is unchanged.
func TestEnrolmentState_RoundTripsThePreviousEncoderOutput(t *testing.T) {
	var st hostEnrolmentState
	require.NoError(t, json.Unmarshal([]byte(previousEnrolmentJSON), &st))
	assert.Nil(t, st.Unknown)
	require.NotNil(t, st.Current)
	assert.Nil(t, st.Current.Unknown)
	out, err := json.Marshal(st)
	require.NoError(t, err)
	assert.Equal(t, previousEnrolmentJSON, string(out))
}

// Members a newer manager wrote, at every nesting level, survive a
// read-modify-write; a known member the manager cleared is not resurrected.
func TestEnrolmentState_KeepsUnknownMembersAtEveryLevel(t *testing.T) {
	doc := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(previousEnrolmentJSON), &doc))
	doc["minManagerVersion"] = "0.1.0"
	doc["futureTop"] = map[string]any{"deep": []any{1, "two"}}
	sub := func(path ...string) map[string]any {
		m := doc
		for _, p := range path {
			next, ok := m[p].(map[string]any)
			require.True(t, ok, p)
			m = next
		}
		return m
	}
	sub("current")["futureCurrent"] = "c"
	sub("current", "record")["futureRecord"] = true
	sub("authorised", "record")["futureAuthorisedRecord"] = 7
	sub("pending")["futurePending"] = "p"
	sub("rekey")["futureRekey"] = "r"
	sub("lastDenial")["futureDenial"] = "d"
	sub("renewal")["futureRenewal"] = "n"
	attempts, ok := doc["renewalAttempts"].([]any)
	require.True(t, ok)
	attempt, ok := attempts[0].(map[string]any)
	require.True(t, ok)
	attempt["futureAttempt"] = "a"
	in, err := json.Marshal(doc)
	require.NoError(t, err)

	var st hostEnrolmentState
	require.NoError(t, json.Unmarshal(in, &st))
	// Modify: retire the pending record (cleared) and move the epoch.
	st.retireOpenRecords()
	st.LastIssuedEpoch = 4
	st.RenewalAttempts = append([]renewalAttempt{{KeyPin: "k4"}}, st.RenewalAttempts...)
	out, err := json.Marshal(st)
	require.NoError(t, err)
	again, err := json.Marshal(st)
	require.NoError(t, err)
	assert.Equal(t, string(out), string(again), "the output is deterministic")

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	doc = got
	assert.Equal(t, "0.1.0", doc["minManagerVersion"])
	assert.Equal(t, map[string]any{"deep": []any{float64(1), "two"}}, doc["futureTop"])
	assert.Equal(t, "c", sub("current")["futureCurrent"])
	assert.Equal(t, true, sub("current", "record")["futureRecord"])
	assert.Equal(t, "r", sub("rekey")["futureRekey"])
	assert.Equal(t, "d", sub("lastDenial")["futureDenial"])
	assert.Equal(t, "n", sub("renewal")["futureRenewal"])
	assert.EqualValues(t, 4, doc["lastIssuedEpoch"])
	assert.NotContains(t, doc, "pending", "a cleared known member is not resurrected")
	assert.NotContains(t, doc, "authorised")
	attempts, ok = doc["renewalAttempts"].([]any)
	require.True(t, ok)
	require.Len(t, attempts, 2)
	first, ok := attempts[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, first, "futureAttempt", "an unknown member stays with its record")
	second, ok := attempts[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", second["futureAttempt"])
}

// A member that differs from a known one only in case is taken by the field,
// as encoding/json does, and never written twice.
func TestEnrolmentState_CaseFoldedMemberIsNotDuplicated(t *testing.T) {
	var st hostEnrolmentState
	require.NoError(t, json.Unmarshal([]byte(`{"HOSTUID":"u","identity":"i"}`), &st))
	assert.Equal(t, "u", st.HostUID)
	assert.Nil(t, st.Unknown)
}

// The store state keeps unknown members at the top level and on each store
// record, which follow their store when the records move.
func TestStoreState_KeepsUnknownMembers(t *testing.T) {
	enrol := strings.Repeat("c3", 16)
	storeA, storeB := strings.Repeat("0a", 16), strings.Repeat("0b", 16)
	head := strings.Repeat("11", 32)
	in := `{"current":{"storeID":"` + storeA + `","enrolmentID":"` + enrol + `","seq":5,"head":"` + head + `","futureRecord":{"x":1}},` +
		`"futureTop":"t","minManagerVersion":"0.0.1"}`
	st, err := decodeStoreState([]byte(in))
	require.NoError(t, err)
	enrolID, err := hostidentity.DecodeID(enrol)
	require.NoError(t, err)
	var idB [16]byte
	for i := range idB {
		idB[i] = 0x0b
	}
	st.Previous = append([]hostidentity.StoreRecord{st.Current}, st.Previous...)
	st.Current = hostidentity.StoreRecord{StoreID: idB, EnrolmentID: enrolID, Seq: 1, Head: [32]byte{2}}
	out, err := encodeStoreState(st)
	require.NoError(t, err)

	var got struct {
		Current  map[string]json.RawMessage   `json:"current"`
		Previous []map[string]json.RawMessage `json:"previous"`
		Top      string                       `json:"futureTop"`
		Floor    string                       `json:"minManagerVersion"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "t", got.Top)
	assert.Equal(t, "0.0.1", got.Floor)
	assert.NotContains(t, got.Current, "futureRecord")
	assert.JSONEq(t, `"`+storeB+`"`, string(got.Current["storeID"]))
	require.Len(t, got.Previous, 1)
	assert.JSONEq(t, `{"x":1}`, string(got.Previous[0]["futureRecord"]))

	// Without unknown members the output is the previous format.
	plain := `{"current":{"storeID":"` + storeA + `","enrolmentID":"` + enrol + `","seq":5,"head":"` + head + `"}}`
	st, err = decodeStoreState([]byte(plain))
	require.NoError(t, err)
	out, err = encodeStoreState(st)
	require.NoError(t, err)
	assert.Equal(t, plain, string(out))
}

// The downgrade floor: refused above this manager's version, when malformed,
// and whenever this manager's version is not a semantic version.
func TestCheckManagerFloor(t *testing.T) {
	cases := []struct {
		name, own, doc string
		refused        bool
	}{
		{"no floor", "v0.4.2", `{"a":1}`, false},
		{"not an object", "v0.4.2", `not json`, false},
		{"empty", "v0.4.2", ``, false},
		{"floor below", "v0.4.2", `{"minManagerVersion":"0.4.1"}`, false},
		{"floor equal", "v0.4.2", `{"minManagerVersion":"v0.4.2"}`, false},
		{"floor above", "v0.4.2", `{"minManagerVersion":"0.5.0"}`, true},
		{"pre-release below its release", "v0.5.0-rc.1", `{"minManagerVersion":"0.5.0"}`, true},
		{"described build below its tag's release", "v0.4.2-3-gabcdef0", `{"minManagerVersion":"0.4.2"}`, true},
		{"described build above an earlier floor", "v0.4.2-3-gabcdef0", `{"minManagerVersion":"0.4.1"}`, false},
		{"floor not a string", "v0.4.2", `{"minManagerVersion":5}`, true},
		{"floor null", "v0.4.2", `{"minManagerVersion":null}`, true},
		{"floor empty", "v0.4.2", `{"minManagerVersion":""}`, true},
		{"floor not semver", "v0.4.2", `{"minManagerVersion":"latest"}`, true},
		{"development build with a floor", "dev", `{"minManagerVersion":"0.0.1"}`, true},
		{"development build without a floor", "dev", `{"a":1}`, false},
		{"untagged build with a floor", "abcdef0", `{"minManagerVersion":"0.0.1"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinBuildVersion(t, tc.own)
			err := checkManagerFloor("doc", []byte(tc.doc))
			if tc.refused {
				require.ErrorIs(t, err, errManagerTooOld)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// compatFixture is a Host with an agent identity and a pins Secret.
func compatFixture(t *testing.T, enrolment string, extra map[string][]byte, objs ...client.Object) (client.Client, *HostEnrolmentConfig,
	*infravirtrigaudiov1beta1.Host, *corev1.Secret) {
	t.Helper()
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-1"
	h.Spec.Identity = "serial-1"
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test"}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostEnrolmentSecretName("default", "host-a"), Namespace: cfg.Namespace,
		Labels: map[string]string{k8sLabelComponent: hostEnrolmentComponent, k8sLabelManagedBy: k8sManagedByVirtRigaud,
			"example.test/other": "kept"},
		Annotations: map[string]string{certrequest.AnnotationHost: "default/host-a", "example.test/note": "kept"}},
		Data: map[string][]byte{hostEnrolmentStateKey: []byte(enrolment)}}
	for k, v := range extra {
		sec.Data[k] = v
	}
	cli := fake.NewClientBuilder().WithScheme(newProviderTLSScheme(t)).WithObjects(append([]client.Object{h, sec}, objs...)...).Build()
	return cli, cfg, h, sec
}

// saveHostEnrolment keeps the data keys, labels and annotations it does not
// own, and still sets its own.
func TestSaveHostEnrolment_KeepsUnknownDataKeys(t *testing.T) {
	ctx := context.Background()
	pinBuildVersion(t, "v0.4.2")
	cli, cfg, h, sec := compatFixture(t, previousEnrolmentJSON, map[string][]byte{
		hostStoreStateKey: []byte(`{}`), "future.json": []byte(`{"minManagerVersion":"0.4.0"}`)})
	l, err := loadHostEnrolment(ctx, cli, cfg, h)
	require.NoError(t, err)
	require.False(t, l.reset)
	l.state.LastIssuedEpoch = 9
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))

	got := &corev1.Secret{}
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sec), got))
	assert.Equal(t, `{"minManagerVersion":"0.4.0"}`, string(got.Data["future.json"]))
	assert.Equal(t, `{}`, string(got.Data[hostStoreStateKey]))
	assert.Contains(t, string(got.Data[hostEnrolmentStateKey]), `"lastIssuedEpoch":9`)
	assert.Equal(t, "kept", got.Labels["example.test/other"])
	assert.Equal(t, hostEnrolmentComponent, got.Labels[k8sLabelComponent])
	assert.Equal(t, "kept", got.Annotations["example.test/note"])

	// A state of another Host object inherits nothing, unknown keys included.
	h.UID = "uid-2"
	l, err = loadHostEnrolment(ctx, cli, cfg, h)
	require.NoError(t, err)
	require.True(t, l.reset)
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sec), got))
	assert.NotContains(t, got.Data, "future.json")
	assert.NotContains(t, got.Data, hostStoreStateKey)
}

// A floor above this manager's version, in either document of the pins
// Secret or in a host ledger Secret of the Host, refuses the Host: nothing
// is loaded, rendered or deleted.
func TestLoadHostEnrolment_RefusesStateRequiringANewerManager(t *testing.T) {
	ctx := context.Background()
	floored := strings.Replace(previousEnrolmentJSON, `{"hostUID"`, `{"minManagerVersion":"9.0.0","hostUID"`, 1)
	ledger := func(host string, data string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ledger-" + strings.ReplaceAll(host, "/", "-"), Namespace: "host-agent-pki",
			Labels: map[string]string{LabelHostLedger: "true"}}, Data: map[string][]byte{"ledger.json": []byte(data)}}
		if host != "" {
			s.Annotations = map[string]string{certrequest.AnnotationHost: host}
		}
		return s
	}
	cases := []struct {
		name      string
		enrolment string
		extra     map[string][]byte
		objs      []client.Object
		refused   bool
	}{
		{"no floor", previousEnrolmentJSON, nil, nil, false},
		{"enrolment floor above", floored, nil, nil, true},
		{"store floor above", previousEnrolmentJSON, map[string][]byte{hostStoreStateKey: []byte(`{"minManagerVersion":"9.0.0"}`)}, nil, true},
		{"store floor malformed", previousEnrolmentJSON, map[string][]byte{hostStoreStateKey: []byte(`{"minManagerVersion":true}`)}, nil, true},
		{"ledger of the Host above", previousEnrolmentJSON, nil, []client.Object{ledger("default/host-a", `{"minManagerVersion":"9.0.0"}`)}, true},
		{"ledger of no Host above", previousEnrolmentJSON, nil, []client.Object{ledger("", `{"minManagerVersion":"9.0.0"}`)}, true},
		{"ledger of the Host malformed", previousEnrolmentJSON, nil, []client.Object{ledger("default/host-a", `{"minManagerVersion":"x"}`)}, true},
		{"ledger of another Host above", previousEnrolmentJSON, nil, []client.Object{ledger("default/host-b", `{"minManagerVersion":"9.0.0"}`)}, false},
		{"ledger of the Host below", previousEnrolmentJSON, nil, []client.Object{ledger("default/host-a", `{"minManagerVersion":"0.1.0"}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinBuildVersion(t, "v0.4.2")
			cli, cfg, h, sec := compatFixture(t, tc.enrolment, tc.extra, tc.objs...)
			_, err := loadHostEnrolment(ctx, cli, cfg, h)
			_, reason := resolveHostAgentTLS(ctx, cli, cfg, h, time.Now())
			delErr := deleteHostEnrolment(ctx, cli, cli, cfg, "default", "host-a", "")
			if !tc.refused {
				require.NoError(t, err)
				require.NoError(t, delErr)
				return
			}
			require.ErrorIs(t, err, errManagerTooOld)
			assert.NotEmpty(t, reason)
			require.ErrorIs(t, delErr, errManagerTooOld)
			require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sec), &corev1.Secret{}), "the pins Secret is kept")
		})
	}
}
