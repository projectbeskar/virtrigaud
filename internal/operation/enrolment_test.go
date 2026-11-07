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

package operation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

const validEnrolmentFile = `{"epoch":{"counter":2,"holder":"000102030405060708090a0b0c0d0e0f"},` +
	`"providers":{"ns/p":{"host-a":"101112131415161718191a1b1c1d1e1f"}}}`

func TestParseEnrolments(t *testing.T) {
	s, err := ParseEnrolments([]byte(validEnrolmentFile))
	require.NoError(t, err)
	e, err := s.Enrolment("ns", "p", "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), e.Epoch.Counter)
	assert.Len(t, e.EnrolmentID, IDLen)
	assert.Len(t, e.Epoch.Holder, IDLen)
	e.EnrolmentID[0] = 0xff
	again, _ := s.Enrolment("ns", "p", "host-a")
	assert.Equal(t, byte(0x10), again.EnrolmentID[0], "Enrolment returns copies")

	_, err = s.Enrolment("ns", "p", "host-b")
	require.ErrorIs(t, err, ErrNoEnrolment)
	_, err = s.Enrolment("other", "p", "host-a")
	require.ErrorIs(t, err, ErrNoEnrolment)
}

func TestParseEnrolments_Refusals(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field":     `{"epoch":{"counter":2,"holder":"000102030405060708090a0b0c0d0e0f"},"providers":{},"extra":1}`,
		"counter zero":      `{"epoch":{"counter":0,"holder":"000102030405060708090a0b0c0d0e0f"},"providers":{}}`,
		"short holder":      `{"epoch":{"counter":1,"holder":"0001"},"providers":{}}`,
		"bad provider key":  `{"epoch":{"counter":1,"holder":"000102030405060708090a0b0c0d0e0f"},"providers":{"p":{}}}`,
		"bad enrolment id":  `{"epoch":{"counter":1,"holder":"000102030405060708090a0b0c0d0e0f"},"providers":{"ns/p":{"h":"zz"}}}`,
		"empty host id":     `{"epoch":{"counter":1,"holder":"000102030405060708090a0b0c0d0e0f"},"providers":{"ns/p":{"":"101112131415161718191a1b1c1d1e1f"}}}`,
		"trailing data":     validEnrolmentFile + `{}`,
		"not json":          `epoch`,
		"nested bad holder": `{"epoch":{"counter":1,"holder":"000102030405060708090a0b0c0d0e0g"},"providers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEnrolments([]byte(data))
			require.Error(t, err)
		})
	}
}

func TestRequiredProtection(t *testing.T) {
	assert.Equal(t, ProtectionIdentity, RequiredProtection(infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor))
	for _, typ := range []infravirtrigaudiov1beta1.ProviderType{
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt,
		infravirtrigaudiov1beta1.ProviderTypeProxmox, "mock", "",
	} {
		assert.Equal(t, ProtectionNone, RequiredProtection(typ), typ)
	}
	assert.Equal(t, "operation-identity", ProtectionIdentity.String())
	assert.Equal(t, "none", ProtectionNone.String())
}
