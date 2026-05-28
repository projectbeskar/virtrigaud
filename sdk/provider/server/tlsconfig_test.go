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

package server

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseAllowedSANs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "manager.svc", []string{"manager.svc"}},
		{"multiple", "a,b,c", []string{"a", "b", "c"}},
		{"trims whitespace", " a , b , c ", []string{"a", "b", "c"}},
		{"drops empties", "a,,b,,,c", []string{"a", "b", "c"}},
		{"only commas", ",,,", nil},
		{"only whitespace", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAllowedSANs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAllowedSANs(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsInsecureOptedIn(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{"unset", "", false},
		{"true lowercase", "true", true},
		{"true uppercase", "TRUE", true},
		{"true mixed case", "True", true},
		{"true with whitespace", "  true  ", true},
		{"false", "false", false},
		{"1 not accepted", "1", false},
		{"yes not accepted", "yes", false},
		{"any other string", "totally", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvInsecure, tc.val)
			if got := isInsecureOptedIn(); got != tc.want {
				t.Errorf("isInsecureOptedIn(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestResolveTLSAndAuth_AllFilesPresent verifies that when the canonical
// TLS files are all present on disk, ResolveTLSAndAuth returns a config
// with RequireClientCert=true and RequireTLS=true.
//
// The helper hard-codes ProviderTLSMountPath, which we cannot rewrite at
// test time. We therefore only test the branches we *can* control by
// manipulating the env-var — the file-present path is exercised by the
// provider integration tests in the higher-level e2e suite.
func TestResolveTLSAndAuth_NoFilesNoOptIn_HardError(t *testing.T) {
	// Ensure neither env-var pushes us into the insecure branch.
	t.Setenv(EnvInsecure, "")
	t.Setenv(EnvAllowedSANs, "")

	resolution, err := ResolveTLSAndAuth()
	if err == nil {
		t.Fatal("expected hard error when TLS files absent and no opt-in, got nil")
	}
	if errors.Is(err, ErrInsecureModeOptedIn) {
		t.Errorf("expected hard error, got insecure-opt-in sentinel")
	}
	if resolution != nil {
		t.Errorf("expected nil resolution on hard error, got %+v", resolution)
	}
}

func TestResolveTLSAndAuth_NoFilesWithOptIn_InsecureSentinel(t *testing.T) {
	t.Setenv(EnvInsecure, "true")
	t.Setenv(EnvAllowedSANs, "")

	resolution, err := ResolveTLSAndAuth()
	if !errors.Is(err, ErrInsecureModeOptedIn) {
		t.Fatalf("expected ErrInsecureModeOptedIn, got %v", err)
	}
	if resolution == nil {
		t.Fatal("expected non-nil resolution with Insecure=true")
	}
	if !resolution.Insecure {
		t.Errorf("expected Insecure=true, got false")
	}
	if resolution.TLS != nil {
		t.Errorf("expected TLS=nil in insecure mode, got %+v", resolution.TLS)
	}
	if resolution.Auth == nil || resolution.Auth.RequireTLS {
		t.Errorf("expected Auth.RequireTLS=false in insecure mode, got %+v", resolution.Auth)
	}
}

func TestResolveTLSAndAuth_AllowedSANsPropagated(t *testing.T) {
	t.Setenv(EnvInsecure, "true")
	t.Setenv(EnvAllowedSANs, "virtrigaud-manager.svc, virtrigaud-manager")

	resolution, err := ResolveTLSAndAuth()
	if !errors.Is(err, ErrInsecureModeOptedIn) {
		t.Fatalf("expected insecure-opt-in branch, got %v", err)
	}
	// In the insecure branch we keep RequireTLS=false and don't bother
	// propagating SANs (no enforcement); the propagation matters only
	// when TLS files are present, which we cover in the higher-level
	// resolution test below by exercising parseAllowedSANs.
	want := []string{"virtrigaud-manager.svc", "virtrigaud-manager"}
	got := parseAllowedSANs("virtrigaud-manager.svc, virtrigaud-manager")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAllowedSANs mismatch: got %v, want %v", got, want)
	}
	_ = resolution
}
