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
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntentAuthenticator(t *testing.T) {
	key, err := DeriveIntentKey(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	a, err := NewIntentAuthenticator(key)
	require.NoError(t, err)
	msg := IntentMessage([]byte("Create"), []byte("request"))
	tag := a.SealIntent(msg)
	assert.Len(t, tag, IntentTagLen)
	assert.True(t, a.OpenIntent(msg, tag))
	assert.False(t, a.OpenIntent(IntentMessage([]byte("Create"), []byte("requesT")), tag), "a changed field fails")
	assert.False(t, a.OpenIntent(msg, nil), "a stripped tag fails")
	assert.False(t, a.OpenIntent(msg, tag[:16]), "a short tag fails")
	assert.True(t, bytes.HasPrefix(msg, []byte(IntentAuthContext)), "domain-separated")

	other, err := DeriveIntentKey(bytes.Repeat([]byte{2}, 32))
	require.NoError(t, err)
	b, err := NewIntentAuthenticator(other)
	require.NoError(t, err)
	assert.False(t, b.OpenIntent(msg, tag), "another key fails")

	_, err = NewIntentAuthenticator(make([]byte, 16))
	require.Error(t, err)
	_, err = DeriveIntentKey(make([]byte, 16))
	require.Error(t, err)
	str, ok := a.(interface{ String() string })
	require.True(t, ok)
	assert.NotContains(t, str.String(), string(key))
}

// Length-prefixing: moving bytes between fields changes the message.
func TestIntentMessage_Unambiguous(t *testing.T) {
	assert.NotEqual(t, IntentMessage([]byte("ab"), []byte("c")), IntentMessage([]byte("a"), []byte("bc")))
	assert.NotEqual(t, IntentMessage([]byte("a")), IntentMessage([]byte("a"), nil))
}

func TestIntentAuthenticatorOf_NoSource(t *testing.T) {
	assert.Nil(t, IntentAuthenticatorOf(nil))
}

// Task tags and intent tags are domain-separated: the same fields never frame
// to the same message.
func TestTaskMessage_SeparatedFromIntents(t *testing.T) {
	f := [][]byte{[]byte("a"), []byte("b")}
	assert.NotEqual(t, IntentMessage(f...), TaskMessage(f...))
	assert.True(t, bytes.HasPrefix(TaskMessage(f...), []byte(TaskAuthContext)))
	assert.False(t, bytes.HasPrefix([]byte(TaskAuthContext), []byte(IntentAuthContext)))
	assert.False(t, bytes.HasPrefix([]byte(IntentAuthContext), []byte(TaskAuthContext)))
}
