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

package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreparedOperation_Redacted: fmt, encoding/json and slog never render
// the request bytes or the salt of a PreparedOperation, alone or inside
// another value.
func TestPreparedOperation_Redacted(t *testing.T) {
	p := PreparedOperation{
		Kind:         OperationKindCreate,
		Identity:     OperationAttemptIdentity{Attempt: 3, OpID: []byte("op-id-bytes-0001")},
		RequestBytes: []byte("#cloud-config\npassword: op-secret"),
		DigestSalt:   []byte("salt-secret-salt-secret-salt-sec"),
	}
	wrapped := struct{ Op PreparedOperation }{p}
	var outs []string
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		outs = append(outs, fmt.Sprintf(f, p), fmt.Sprintf(f, wrapped), fmt.Sprintf(f, &p))
	}
	for _, v := range []any{p, &p, wrapped, []PreparedOperation{p}} {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		outs = append(outs, string(b))
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("op", "prepared", p)
	logger.Info("op", "prepared", &p)
	text := slog.New(slog.NewTextHandler(&buf, nil))
	text.Info("op", "prepared", p)
	outs = append(outs, buf.String())
	for _, out := range outs {
		assert.NotContains(t, out, "op-secret")
		assert.NotContains(t, out, "salt-secret")
		// base64 of the user data must not appear either.
		assert.NotContains(t, out, "I2Nsb3VkLWNvbmZpZw")
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	assert.JSONEq(t, `{"kind":"Create","attempt":3,"requestBytes":33,"redacted":true}`, string(b))
}
