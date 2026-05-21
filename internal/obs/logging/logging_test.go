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
)

// TestRedactorRedact covers the two structurally different redaction shapes
// the package supports:
//
//   - Patterns with a single capture group (URL password), where the captured
//     segment is the secret to redact.
//   - Patterns with two capture groups (key=value), where group 1 is the key
//     name (must be preserved) and group 2 is the secret value (must be
//     redacted). Regression guard for a bug where the key was redacted instead
//     of the value, leaking the secret and obscuring the key name.
func TestRedactorRedact(t *testing.T) {
	r := NewRedactor()

	cases := []struct {
		name             string
		input            string
		mustContain      []string
		mustNotContain   []string
		containRedactTag bool
	}{
		{
			name:             "url password redacted, user preserved",
			input:            "https://admin:hunter2@db.example.com/path",
			mustContain:      []string{"https://admin:", "@db.example.com/path", "[REDACTED]"},
			mustNotContain:   []string{"hunter2"},
			containRedactTag: true,
		},
		{
			name:             "password key=value: value redacted, key preserved",
			input:            "password=hunter2",
			mustContain:      []string{"password=", "[REDACTED]"},
			mustNotContain:   []string{"hunter2"},
			containRedactTag: true,
		},
		{
			name:             "token key:value with quotes: value redacted, key preserved",
			input:            `token: "abc123def"`,
			mustContain:      []string{"token", "[REDACTED]"},
			mustNotContain:   []string{"abc123def"},
			containRedactTag: true,
		},
		{
			name:             "api_key=value: value redacted, key preserved",
			input:            "api_key=xyz789",
			mustContain:      []string{"api_key=", "[REDACTED]"},
			mustNotContain:   []string{"xyz789"},
			containRedactTag: true,
		},
		{
			name:             "secret=value with separator chars",
			input:            "config: secret=topsecret other=fine",
			mustContain:      []string{"secret=", "[REDACTED]", "other=fine"},
			mustNotContain:   []string{"topsecret"},
			containRedactTag: true,
		},
		{
			name:             "ssh public key fully redacted",
			input:            "deploy key: ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ== user@host",
			mustContain:      []string{"[REDACTED]"},
			mustNotContain:   []string{"AAAAB3NzaC1yc2EAAAADAQABAAABAQ=="},
			containRedactTag: true,
		},
		{
			name:             "plain text unchanged",
			input:            "user logged in successfully",
			mustContain:      []string{"user logged in successfully"},
			mustNotContain:   []string{"[REDACTED]"},
			containRedactTag: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Redact(tc.input)

			if tc.containRedactTag && !strings.Contains(got, "[REDACTED]") {
				t.Errorf("Redact(%q) = %q; expected to contain %q", tc.input, got, "[REDACTED]")
			}

			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("Redact(%q) = %q; expected to contain %q", tc.input, got, want)
				}
			}
			for _, banned := range tc.mustNotContain {
				if strings.Contains(got, banned) {
					t.Errorf("Redact(%q) = %q; expected NOT to contain %q (secret leak)",
						tc.input, got, banned)
				}
			}
		})
	}
}

// TestRedactorKeyValuePreservesKey is an explicit regression test for the bug
// where two-capture-group patterns redacted the key (group 1) instead of the
// value (group 2). With the old behavior the output would look like
// "[REDACTED]=hunter2" — both leaking the secret and stripping the key name.
func TestRedactorKeyValuePreservesKey(t *testing.T) {
	r := NewRedactor()
	got := r.Redact("password=hunter2")

	if !strings.Contains(got, "password=") {
		t.Errorf("Redact removed the key name; got %q, want it to start with %q", got, "password=")
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("Redact leaked the secret value; got %q, must not contain %q", got, "hunter2")
	}
	if strings.HasPrefix(got, "[REDACTED]=") {
		t.Errorf("Redact replaced the key instead of the value; got %q", got)
	}
}

// TestRedactorRedactMap confirms that map values are redacted when their keys
// are flagged sensitive by isSensitiveKey, and that other values run through
// the regex-based Redact path.
func TestRedactorRedactMap(t *testing.T) {
	r := NewRedactor()

	in := map[string]string{
		"password": "hunter2",
		"token":    "abc123",
		"username": "alice",
		"note":     "password=embedded-secret",
	}

	out := r.RedactMap(in)

	if out["password"] != "[REDACTED]" {
		t.Errorf("sensitive key %q not redacted; got %q", "password", out["password"])
	}
	if out["token"] != "[REDACTED]" {
		t.Errorf("sensitive key %q not redacted; got %q", "token", out["token"])
	}
	if out["username"] != "alice" {
		t.Errorf("non-sensitive key %q was altered; got %q", "username", out["username"])
	}
	if strings.Contains(out["note"], "embedded-secret") {
		t.Errorf("embedded secret in non-sensitive value not redacted; got %q", out["note"])
	}
}

// TestRedactorNilMap guards against a panic when RedactMap is called with a
// nil input map.
func TestRedactorNilMap(t *testing.T) {
	r := NewRedactor()
	if out := r.RedactMap(nil); out != nil {
		t.Errorf("RedactMap(nil) = %v; want nil", out)
	}
}
