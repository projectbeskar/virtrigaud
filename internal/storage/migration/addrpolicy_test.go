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

package migration

import (
	"net"
	"testing"
)

// TestHostPolicy_AlwaysDenied verifies the SSRF-dangerous targets are rejected
// even with no allowlist configured — most importantly the 169.254.169.254
// cloud-metadata address (ADR-0006 C3).
func TestHostPolicy_AlwaysDenied(t *testing.T) {
	p, err := NewHostPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{
		"127.0.0.1", "127.0.0.53", // loopback
		"169.254.169.254", "169.254.1.1", // link-local incl. cloud metadata
		"::1",       // ipv6 loopback
		"0.0.0.0",   // unspecified
		"224.0.0.1", // multicast
		"ff02::1",   // ipv6 multicast
		"fe80::1",   // ipv6 link-local
	} {
		if err := p.ValidateHost(h); err == nil {
			t.Errorf("ValidateHost(%q) = nil, want denied", h)
		}
	}
}

// TestHostPolicy_PermissiveDefault verifies that without an allowlist, ordinary
// public and RFC1918 addresses are permitted (so on-prem/lab storage works).
func TestHostPolicy_PermissiveDefault(t *testing.T) {
	p, _ := NewHostPolicy(nil)
	for _, h := range []string{"172.16.56.13", "10.0.0.5", "192.168.1.1", "8.8.8.8"} {
		if err := p.ValidateHost(h); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want allowed (no allowlist)", h, err)
		}
	}
}

// TestHostPolicy_Allowlist verifies that a configured allowlist permits only its
// members, and that the always-denied set still wins inside the allowlist.
func TestHostPolicy_Allowlist(t *testing.T) {
	p, err := NewHostPolicy([]string{"172.16.56.0/24", "10.1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"172.16.56.13", "172.16.56.1", "10.1.2.3"} {
		if err := p.ValidateHost(h); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want allowed", h, err)
		}
	}
	for _, h := range []string{"10.0.0.5", "8.8.8.8", "172.16.57.1"} {
		if err := p.ValidateHost(h); err == nil {
			t.Errorf("ValidateHost(%q) = nil, want denied (outside allowlist)", h)
		}
	}
	if err := p.ValidateHost("169.254.169.254"); err == nil {
		t.Error("metadata IP allowed under allowlist; always-denied must win")
	}
}

// TestNewHostPolicy_BadEntry verifies a non-IP/CIDR allowlist entry is rejected.
func TestNewHostPolicy_BadEntry(t *testing.T) {
	if _, err := NewHostPolicy([]string{"not-an-ip"}); err == nil {
		t.Error("NewHostPolicy(bad) = nil err, want error")
	}
}

// TestValidateS3Endpoint covers the URL / bare-host / scheme forms of an S3
// endpoint against the SSRF gate.
func TestValidateS3Endpoint(t *testing.T) {
	p, _ := NewHostPolicy(nil)
	if err := p.ValidateS3Endpoint("http://169.254.169.254:9000"); err == nil {
		t.Error("metadata S3 endpoint allowed, want denied")
	}
	if err := p.ValidateS3Endpoint("http://127.0.0.1:9000"); err == nil {
		t.Error("localhost S3 endpoint allowed, want denied")
	}
	if err := p.ValidateS3Endpoint("172.16.56.13:9000"); err != nil {
		t.Errorf("private S3 endpoint denied: %v", err)
	}
	if err := p.ValidateS3Endpoint("https://172.16.56.13"); err != nil {
		t.Errorf("private https S3 endpoint denied: %v", err)
	}
}

// TestValidateHost_HostnameResolution verifies hostnames are resolved and ALL
// resolved addresses are checked (a host resolving to even one forbidden address
// is rejected — a DNS-rebind-style guard at validate time).
func TestValidateHost_HostnameResolution(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "evil.lab":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "good.lab":
			return []net.IP{net.ParseIP("172.16.56.13")}, nil
		case "mixed.lab":
			return []net.IP{net.ParseIP("172.16.56.13"), net.ParseIP("127.0.0.1")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	p, _ := NewHostPolicy(nil)
	if err := p.ValidateHost("evil.lab"); err == nil {
		t.Error("hostname resolving to metadata IP allowed, want denied")
	}
	if err := p.ValidateHost("good.lab"); err != nil {
		t.Errorf("hostname resolving to private IP denied: %v", err)
	}
	if err := p.ValidateHost("mixed.lab"); err == nil {
		t.Error("hostname resolving to a forbidden IP allowed, want denied")
	}
	if err := p.ValidateHost("nope.lab"); err == nil {
		t.Error("unresolvable host allowed, want error")
	}
}
