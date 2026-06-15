/*
Copyright 2025.

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

package logging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactStringValuesNotKeys is the regression canary for issue #95.
//
// The pre-fix implementation replaced the FIRST capture group of every
// pattern. For two-group patterns like (key)(value), that meant the field
// NAME got masked while the SECRET VALUE survived in cleartext. This test
// asserts the inverse: the value is redacted and the field name is preserved
// (the field name is operationally useful — knowing "there is a password
// here" is fine; what matters is not leaking the password itself).
func TestRedactStringValuesNotKeys(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		mustNotContain []string // secret values that MUST NOT appear in output
		mustContain    []string // tokens that MUST appear in output ([REDACTED] markers, preserved context)
	}{
		{
			name:           "password=value (equals separator)",
			input:          "password=hunter2",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"password", "[REDACTED]"},
		},
		{
			name:           "password: value (colon separator)",
			input:          "password: hunter2",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"password", "[REDACTED]"},
		},
		{
			name:           "api_key (underscore variant)",
			input:          "api_key=abc123xyz",
			mustNotContain: []string{"abc123xyz"},
			mustContain:    []string{"api_key", "[REDACTED]"},
		},
		{
			name:           "api-key (hyphen variant)",
			input:          "api-key=abc123xyz",
			mustNotContain: []string{"abc123xyz"},
			mustContain:    []string{"api-key", "[REDACTED]"},
		},
		{
			name:           "token (case-insensitive)",
			input:          "TOKEN=very-secret-token-value",
			mustNotContain: []string{"very-secret-token-value"},
			mustContain:    []string{"TOKEN", "[REDACTED]"},
		},
		{
			name:           "passwd alias",
			input:          "passwd=hunter2",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"passwd", "[REDACTED]"},
		},
		{
			name:           "pwd alias",
			input:          "pwd=hunter2",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"pwd", "[REDACTED]"},
		},
		{
			name:           "secret keyword",
			input:          "secret=value-to-hide",
			mustNotContain: []string{"value-to-hide"},
			mustContain:    []string{"secret", "[REDACTED]"},
		},
		{
			name:           "quoted value (double quotes)",
			input:          `password="hunter2"`,
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"password", "[REDACTED]"},
		},
		{
			name:           "quoted value (single quotes)",
			input:          `password='hunter2'`,
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"password", "[REDACTED]"},
		},
		{
			name:           "multiple kv pairs (the original integration-test failure case)",
			input:          "password=secret123 and api_key=abcdef",
			mustNotContain: []string{"secret123", "abcdef"},
			mustContain:    []string{"password", "api_key", "[REDACTED]"},
		},
		{
			name:           "URL with embedded password",
			input:          "https://user:hunter2@host.example",
			mustNotContain: []string{"hunter2"},
			mustContain:    []string{"https://user:", "@host.example", "[REDACTED]"},
		},
		{
			name:           "qemu+ssh URL with password",
			input:          "qemu+ssh://wrkode:KUb34us!@172.16.56.8/system",
			mustNotContain: []string{"KUb34us!"},
			mustContain:    []string{"qemu+ssh://wrkode:", "@172.16.56.8/system", "[REDACTED]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactString(tt.input)
			for _, leak := range tt.mustNotContain {
				assert.NotContains(t, got, leak,
					"input=%q produced output that still contains the secret %q", tt.input, leak)
			}
			for _, marker := range tt.mustContain {
				assert.Contains(t, got, marker,
					"input=%q produced output missing expected marker %q (got %q)", tt.input, marker, got)
			}
		})
	}
}

// TestRedactStringNonSensitiveInputUntouched verifies the redactor doesn't
// over-redact harmless content. Currently the "base64-like string > 20 chars"
// pattern is broad enough that some long alphanumeric tokens (e.g. UUIDs
// without hyphens, container image digests) will be replaced — that's a known
// design trade-off but worth pinning to catch unintentional regressions.
func TestRedactStringNonSensitiveInputUntouched(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "short alphanumeric",
			input: "hello world 12345",
			want:  "hello world 12345",
		},
		{
			name:  "log line without secrets",
			input: "Reconcile VirtualMachine default/test-vm: phase=Provisioning",
			want:  "Reconcile VirtualMachine default/test-vm: phase=Provisioning",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactString(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRedactMap verifies map-based redaction redacts whole VALUES for
// sensitive keys, and recursively redacts values for non-sensitive keys.
func TestRedactMap(t *testing.T) {
	input := map[string]string{
		"password":    "hunter2",
		"username":    "wrkode",
		"api_key":     "sk-abcd1234567890",
		"description": "harmless field",
		"connection":  "password=secret-in-value",
	}

	got := RedactMap(input)

	assert.Equal(t, "[REDACTED]", got["password"],
		"sensitive key 'password' should have entire value redacted")
	assert.Equal(t, "[REDACTED]", got["api_key"],
		"sensitive key 'api_key' should have entire value redacted")
	assert.Equal(t, "wrkode", got["username"],
		"non-sensitive key 'username' should not be redacted (it's not in isSensitiveKey list)")
	assert.Equal(t, "harmless field", got["description"],
		"non-sensitive key with non-sensitive value should pass through unchanged")
	// The "connection" value contains a kv-pair pattern that the value-level
	// Redact() should catch.
	assert.NotContains(t, got["connection"], "secret-in-value",
		"value-level redaction should still apply to non-sensitive keys when their value matches a pattern")
}

// TestRedactStringSSHPublicKey verifies SSH public keys are scrubbed.
func TestRedactStringSSHPublicKey(t *testing.T) {
	input := "authorized: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7lHIuo2QJBkdVDL79bl wrkode@laptop\nnext line"
	got := RedactString(input)
	assert.NotContains(t, got, "AAAAC3NzaC1lZDI1NTE5AAAAIN7lHIuo2QJBkdVDL79bl",
		"SSH public key body must be redacted (got %q)", got)
}

// TestIsSensitiveKey covers the helper used by RedactMap.
func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{"password", "Password", "PASSWORD", "api_key", "apikey",
		"access_key", "private_key", "tls.key", "client.key", "ssh_private_key",
		"userdata", "user_data", "secret", "token", "credential", "auth"}
	for _, k := range sensitive {
		assert.True(t, isSensitiveKey(k), "expected %q to be classified as sensitive", k)
	}

	nonSensitive := []string{"username", "host", "port", "namespace", "name", "phase"}
	for _, k := range nonSensitive {
		assert.False(t, isSensitiveKey(k),
			"expected %q to NOT be classified as sensitive", k)
	}
}

// TestRedactStringIdempotent verifies running Redact twice doesn't double-mask.
func TestRedactStringIdempotent(t *testing.T) {
	input := "password=hunter2 and api_key=abc"
	once := RedactString(input)
	twice := RedactString(once)
	// After the first pass values are gone; second pass should be a no-op
	// (the [REDACTED] marker shouldn't itself match any pattern).
	assert.Equal(t, once, twice,
		"redaction should be idempotent: once=%q twice=%q", once, twice)
	assert.False(t, strings.Contains(twice, "hunter2"),
		"secret must still be absent after second pass")
}

// TestRedactMapHidesS3MigrationCredentials is the ADR-0006 no-leak canary: the
// S3 migration credential map keys (accessKeyID/secretAccessKey/sessionToken)
// must be redacted by key so credential VALUES never reach logs/events/Status.
func TestRedactMapHidesS3MigrationCredentials(t *testing.T) {
	in := map[string]string{
		"accessKeyID":     "AKIAEXAMPLE1234567",
		"secretAccessKey": "ZmFrZS1zZWNyZXQtbm90LXJlYWw",
		"sessionToken":    "FQoGZXIvYXdzEXAMPLEtoken",
		"backend":         "s3", // non-sensitive context survives
	}
	out := RedactMap(in)
	for _, k := range []string{"accessKeyID", "secretAccessKey", "sessionToken"} {
		assert.Equal(t, "[REDACTED]", out[k], "value for %q must be redacted", k)
	}
	assert.Equal(t, "s3", out["backend"], "non-sensitive context should be preserved")
	// Belt-and-suspenders: the raw secret material must not appear anywhere.
	joined := strings.Join([]string{out["accessKeyID"], out["secretAccessKey"], out["sessionToken"]}, " ")
	for _, secret := range []string{"AKIAEXAMPLE1234567", "ZmFrZS1zZWNyZXQtbm90LXJlYWw", "FQoGZXIvYXdzEXAMPLEtoken"} {
		assert.False(t, strings.Contains(joined, secret), "secret %q leaked", secret)
	}
}
