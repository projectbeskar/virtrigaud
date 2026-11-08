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

package certrequest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseIssuerRef(t *testing.T) {
	r, err := ParseIssuerRef("ClusterIssuer/host-agent-ca")
	require.NoError(t, err)
	assert.Equal(t, IssuerRef{Group: Group, Kind: "ClusterIssuer", Name: "host-agent-ca"}, r)
	r, err = ParseIssuerRef("pki.example.test/ExternalIssuer/enterprise")
	require.NoError(t, err)
	assert.Equal(t, "pki.example.test/ExternalIssuer/enterprise", r.String())
	for _, bad := range []string{"", "name", "a//b", "a/b/c/d", "/x"} {
		_, err := ParseIssuerRef(bad)
		assert.Error(t, err, bad)
	}
}

func TestNewAndReadBack(t *testing.T) {
	cr := New(Spec{Namespace: "pki", GenerateName: "e-", HostNamespace: "infra", HostName: "host-a",
		IssuerRef: IssuerRef{Group: Group, Kind: "Issuer", Name: "ca"}, CSRPEM: []byte("csr"), Duration: 24 * time.Hour})
	assert.True(t, IsEnrolment(cr))
	ns, name, ok := HostOf(cr)
	assert.True(t, ok)
	assert.Equal(t, "infra/host-a", ns+"/"+name)
	assert.Equal(t, IssuerRef{Group: Group, Kind: "Issuer", Name: "ca"}, IssuerRefOf(cr))
	req, err := RequestOf(cr)
	require.NoError(t, err)
	assert.Equal(t, []byte("csr"), req.CSRPEM)
	assert.Equal(t, 24*time.Hour, req.Duration)
	assert.False(t, req.IsCA)

	cr.SetAnnotations(map[string]string{AnnotationHost: "no-slash"})
	_, _, ok = HostOf(cr)
	assert.False(t, ok)
}

func TestConditions(t *testing.T) {
	cr := NewObject()
	assert.False(t, Decided(cr))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, SetCondition(cr, ConditionApproved, StatusTrue, "R", "m", t0))
	require.NoError(t, SetCondition(cr, ConditionApproved, StatusTrue, "R2", "m2", t0.Add(time.Hour)))
	conds, _ := cr.Object["status"].(map[string]any)["conditions"].([]any)
	require.Len(t, conds, 1, "a condition is replaced, not duplicated")
	c, _ := conds[0].(map[string]any)
	assert.Equal(t, t0.Format(time.RFC3339), c["lastTransitionTime"], "the transition time changes only with the status")
	assert.True(t, Decided(cr))
	assert.False(t, Failed(cr))
	require.NoError(t, SetCondition(cr, ConditionReady, StatusFalse, ReasonFailed, "x", t0))
	assert.True(t, Failed(cr))

	require.NoError(t, SetCertificate(cr, []byte("cert"), nil))
	got, err := Certificate(cr)
	require.NoError(t, err)
	assert.Equal(t, []byte("cert"), got)
}
