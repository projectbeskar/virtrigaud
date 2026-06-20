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
	"fmt"
	"net"
	"net/url"
	"strings"
)

// lookupIP resolves a hostname to its IP addresses. It is a package variable so
// tests can stub DNS without touching the network.
var lookupIP = net.LookupIP

// HostPolicy decides whether a migration staging backend's network address — an
// S3 endpoint host or an NFS server — may be dialed. A VMMigration's
// `storage.s3.endpoint` / `storage.nfs.server` are tenant-controlled and are
// dialed by the provider pod (S3 relay) or, for NFS `direct` mode, by the
// hypervisor host. Without this gate a migration could coerce those processes
// into connecting to internal services or the cloud-metadata endpoint (SSRF).
// See ADR-0006 (Slice 4 security, condition C3).
//
// The policy ALWAYS denies the SSRF-dangerous, never-legitimate-as-storage
// targets — loopback, link-local (including the 169.254.169.254 cloud-metadata
// address and fe80::/10), the unspecified address, and multicast — regardless of
// configuration, so the SSRF teeth are removed even without operator setup. When
// an operator allowlist is configured, an address must ALSO fall within it.
// RFC1918 / on-prem ranges are permitted by default (migration storage is
// commonly on a private network); regulated deployments SHOULD configure an
// allowlist to lock egress down further.
type HostPolicy struct {
	allow           []*net.IPNet
	allowConfigured bool
}

// NewHostPolicy builds a HostPolicy from operator-supplied allowlist entries,
// each an IP or CIDR. An empty/nil list yields a permissive-except-always-denied
// policy. Hostnames are not accepted as allowlist entries — request hosts are
// resolved to IPs and matched against these ranges.
func NewHostPolicy(allow []string) (*HostPolicy, error) {
	p := &HostPolicy{}
	for _, raw := range allow {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		p.allowConfigured = true
		if _, ipnet, err := net.ParseCIDR(entry); err == nil {
			p.allow = append(p.allow, ipnet)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			p.allow = append(p.allow, singleHostNet(ip))
			continue
		}
		return nil, fmt.Errorf("invalid storage-host allowlist entry %q (want an IP or CIDR)", entry)
	}
	return p, nil
}

// singleHostNet returns a /32 (IPv4) or /128 (IPv6) net for a single IP.
func singleHostNet(ip net.IP) *net.IPNet {
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
}

// ValidateS3Endpoint validates the host of an S3 endpoint, which may be a URL
// ("http://host:port"), a bare "host:port", or "host". An empty endpoint means
// the AWS default (validated as s3.amazonaws.com). This is the S3 entry point
// for the SSRF gate.
func (p *HostPolicy) ValidateS3Endpoint(endpoint string) error {
	host := hostFromS3Endpoint(endpoint)
	if host == "" {
		host = "s3.amazonaws.com"
	}
	return p.ValidateHost(host)
}

// ValidateHost resolves host — a hostname or IP literal, optionally with a port
// or IPv6 brackets — and rejects it if any resolved address is always-denied,
// or, when an allowlist is configured, falls outside it. All resolved addresses
// must pass (a host that resolves to even one forbidden address is rejected).
func (p *HostPolicy) ValidateHost(host string) error {
	h := strings.TrimSpace(host)
	if h == "" {
		return fmt.Errorf("storage host is empty")
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(h, "[]")

	ips, err := resolveHost(h)
	if err != nil {
		return fmt.Errorf("cannot resolve storage host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("storage host %q resolved to no addresses", h)
	}
	for _, ip := range ips {
		if isAlwaysDenied(ip) {
			return fmt.Errorf("storage host %q resolves to forbidden address %s (loopback/link-local/metadata/multicast are blocked)", h, ip)
		}
		if p.allowConfigured && !p.inAllowlist(ip) {
			return fmt.Errorf("storage host %q (%s) is not in the configured storage-host allowlist", h, ip)
		}
	}
	return nil
}

// inAllowlist reports whether ip falls within any configured allow CIDR.
func (p *HostPolicy) inAllowlist(ip net.IP) bool {
	for _, n := range p.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isAlwaysDenied reports whether ip is an SSRF-dangerous target that is never a
// legitimate migration storage endpoint. IsLinkLocalUnicast covers the
// 169.254.169.254 cloud-metadata address and fe80::/10.
func isAlwaysDenied(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast()
}

// resolveHost returns the IPs for host, short-circuiting IP literals so no DNS
// lookup is performed for them.
func resolveHost(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return lookupIP(host)
}

// hostFromS3Endpoint extracts host[:port] from an S3 endpoint that may be a URL,
// a bare "host:port", or "host". An empty endpoint returns "".
func hostFromS3Endpoint(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return ""
	}
	if strings.Contains(e, "://") {
		if u, err := url.Parse(e); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return e
}
