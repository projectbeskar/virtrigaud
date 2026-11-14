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

package grantverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestCheckOuter: the outer-request rules alone, as Verify applies them
// first; nothing inside the envelope is judged.
func TestCheckOuter(t *testing.T) {
	refused := func(m proto.Message) *grantverify.Refusal {
		t.Helper()
		_, err := grantverify.CheckOuter(m)
		require.Error(t, err)
		r, ok := grantverify.AsRefusal(err)
		require.True(t, ok, "a *Refusal: %v", err)
		return r
	}

	r := refused(&providerv1.PowerRequest{Id: "vm", TargetHostId: "host-a"})
	assert.Equal(t, grantverify.ReasonIdentityRequired, r.Reason)
	assert.Equal(t, grantverify.RuleEnvelopeMissing, r.Rule)
	assert.False(t, r.InvalidArgument)

	env := &providerv1.OperationEnvelope{Request: []byte("not even a request")}
	r = refused(&providerv1.PowerRequest{OperationEnvelope: env, TargetHostId: "host-a"})
	assert.Equal(t, grantverify.RuleOuterFieldSet, r.Rule)
	assert.True(t, r.InvalidArgument)

	withUnknown := &providerv1.DeleteRequest{OperationEnvelope: env}
	withUnknown.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1))
	r = refused(withUnknown)
	assert.Equal(t, grantverify.RuleOuterFieldSet, r.Rule)
	assert.True(t, r.InvalidArgument)

	r = refused(&providerv1.DescribeRequest{Id: "vm"})
	assert.Equal(t, grantverify.RuleNotMutatingRequest, r.Rule)

	// An envelope with no grant and undecodable request bytes passes: the
	// host judges it.
	got, err := grantverify.CheckOuter(&providerv1.PowerRequest{OperationEnvelope: env})
	require.NoError(t, err)
	assert.Same(t, env, got)
}
