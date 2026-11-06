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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// FuzzVerify_MutatedGrant: flipping any byte of the body, the signature,
// either certificate or the request of a valid call never panics and is
// never accepted.
func FuzzVerify_MutatedGrant(f *testing.F) {
	pki := validPKI(f)
	v := testVerifier(f, pki)
	r := MintRequest{
		Identity:   testIdentity(providerv1.OperationKind_OPERATION_KIND_POWER_ON),
		DigestSalt: bytesOf(0x5A, SaltLen),
		IssuedAt:   testEpoch,
	}
	var err error
	r.Request, err = proto.Marshal(testRequest(providerv1.OperationKind_OPERATION_KIND_POWER_ON))
	require.NoError(f, err)
	grant, err := MintOperationGrant(context.Background(), pki, r)
	require.NoError(f, err)
	for target := uint8(0); target < 5; target++ {
		for _, pos := range []uint{0, 1, 7, 40, 100, 300} {
			f.Add(target, pos, byte(0x01))
			f.Add(target, pos, byte(0x80))
		}
	}
	f.Fuzz(func(t *testing.T, target uint8, pos uint, flip byte) {
		if flip == 0 {
			return
		}
		g := mustAs[*providerv1.OperationGrant](t, proto.Clone(grant))
		req := append([]byte(nil), r.Request...)
		var buf []byte
		switch target % 5 {
		case 0:
			buf = g.Body
		case 1:
			buf = g.Signature
		case 2:
			buf = g.SignerCertChain[0]
		case 3:
			buf = g.SignerCertChain[1]
		default:
			buf = req
		}
		if len(buf) == 0 {
			return
		}
		buf[int(pos%uint(len(buf)))] ^= flip
		out := &providerv1.PowerRequest{OperationEnvelope: &providerv1.OperationEnvelope{Request: req, Grant: g}}
		got, err := v.Verify(providerv1.Provider_Power_FullMethodName, out)
		if err == nil {
			t.Fatalf("a mutated call (target %d, pos %d) was accepted: kind %s", target%5, pos, got.Kind)
		}
		if _, ok := AsRefusal(err); !ok {
			t.Fatalf("not a Refusal: %v", err)
		}
	})
}

// FuzzVerify_ArbitraryRequest: an arbitrary outer request, decoded as each
// mutating request type, never panics the verifier and is never accepted.
func FuzzVerify_ArbitraryRequest(f *testing.F) {
	pki := validPKI(f)
	v := testVerifier(f, pki)
	c := newCall(f, pki, providerv1.OperationKind_OPERATION_KIND_DELETE)
	seed, err := proto.Marshal(c.outer(f, pki))
	require.NoError(f, err)
	f.Add(uint8(1), seed)
	f.Add(uint8(0), []byte{})
	f.Add(uint8(2), []byte{0x32, 0x00})
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		rows := Rows()
		row := rows[int(which)%len(rows)]
		m := row.NewRequest()
		if proto.Unmarshal(data, m) != nil {
			return
		}
		got, err := v.Verify(row.FullMethod, m)
		if err == nil && !proto.Equal(got.Request, testRequest(providerv1.OperationKind_OPERATION_KIND_DELETE)) {
			t.Fatalf("an arbitrary request was accepted as %s", got.Kind)
		}
	})
}
