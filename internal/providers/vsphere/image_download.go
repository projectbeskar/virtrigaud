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

package vsphere

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The OVA download client.
//
// ImagePrepare fetches a tenant-supplied URL from inside the provider pod. The
// client used for it therefore:
//
//   - never connects to a loopback, link-local (169.254.0.0/16 and fe80::/10,
//     which includes cloud metadata endpoints), unspecified (0.0.0.0/8, ::) or
//     multicast address — checked on the resolved address of every
//     connection, redirects included, so DNS cannot route around it; private
//     (RFC 1918 / unique-local) addresses stay allowed, since image servers are
//     usually internal;
//   - follows at most maxImageDownloadRedirects redirects, each re-checked for
//     an http(s) scheme and an allowed address, and never from https down to
//     plain http;
//   - bounds the TCP connect, TLS handshake and response-header waits (the
//     body itself may take long: images are large);
//   - reads at most the provider's download limit
//     (VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB);
//   - abandons a body that moves less than minDownloadBytesPerWindow in any
//     imageDownloadStallWindow (the throughput watchdog), and the whole
//     prepare gives up importDeadlineMargin before the manager's deadline.
//
// Image downloads connect directly: the process-wide HTTP_PROXY/HTTPS_PROXY
// (which the vCenter SOAP client honours) are ignored for them. Only when a
// proxy is set explicitly for image downloads (VIRTRIGAUD_VSPHERE_IMAGE_PROXY)
// does the provider connect to that proxy, which then resolves named targets
// itself — so the per-connection address check sees the proxy, not the target:
// restrict the proxy's egress too. An IP-literal or localhost target, and a
// redirect to one, is refused before any request either way.

const (
	// maxImageDownloadGiBEnv sets the largest image, in GiB, the vSphere
	// provider downloads for ImagePrepare.
	maxImageDownloadGiBEnv = "VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB"
	// defaultMaxImageDownloadGiB is the download limit when the environment
	// sets none, or an invalid one.
	defaultMaxImageDownloadGiB = 256
	// minMaxImageDownloadGiB and maxMaxImageDownloadGiB bound the configurable
	// download limit.
	minMaxImageDownloadGiB = 1
	maxMaxImageDownloadGiB = 16384
	// bytesPerGiB converts the configured limit to bytes.
	bytesPerGiB = int64(1) << 30

	// maxImageDownloadRedirects is how many redirects an image download
	// follows.
	maxImageDownloadRedirects = 3
	// imageDownloadDialTimeout bounds a TCP connect to the image source.
	imageDownloadDialTimeout = 30 * time.Second
	// imageDownloadTLSHandshakeTimeout bounds the TLS handshake.
	imageDownloadTLSHandshakeTimeout = 15 * time.Second
	// imageDownloadResponseHeaderTimeout bounds the wait for the response
	// headers once the request is sent.
	imageDownloadResponseHeaderTimeout = 60 * time.Second
	// imageDownloadIdleConnTimeout closes idle keep-alive connections.
	imageDownloadIdleConnTimeout = 90 * time.Second

	// localhostName and localhostSuffix name the loopback host.
	localhostName   = "localhost"
	localhostSuffix = ".localhost"
)

// Download throughput watchdog defaults: a download that moves less than
// minDownloadBytesPerWindow in any imageDownloadStallWindow is abandoned. A
// source that sends its headers promptly and then drip-feeds its body would
// otherwise hold the prepare (and the provider's staging space) until the
// manager's deadline, on every retry.
const (
	imageDownloadStallWindow  = 60 * time.Second
	minDownloadBytesPerWindow = 64 << 10
)

// errImageSourceTooSlow is the cause the throughput watchdog cancels a
// download with.
var errImageSourceTooSlow = stderrors.New("the image source sent too little data in the watchdog window")

// atomicCountingReader counts the bytes read through it, for a concurrent
// watchdog.
type atomicCountingReader struct {
	r io.Reader
	n atomic.Int64
}

// Read implements io.Reader.
func (c *atomicCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// downloadWatchdog returns the provider's throughput watchdog window and the
// minimum bytes per window (the defaults for a Provider built without them).
func (p *Provider) downloadWatchdog() (time.Duration, int64) {
	window, minBytes := p.downloadStallWindow, p.downloadMinBytesPerWindow
	if window <= 0 {
		window = imageDownloadStallWindow
	}
	if minBytes <= 0 {
		minBytes = minDownloadBytesPerWindow
	}
	return window, minBytes
}

// watchDownloadThroughput starts the throughput watchdog of a download whose
// body bytes are counted in read: every window, if fewer than the minimum
// bytes arrived since the last check, it cancels ctx with
// errImageSourceTooSlow (which also unblocks a Read waiting on a silent
// source). It stops when stop is called or ctx is done; call stop exactly
// once.
func (p *Provider) watchDownloadThroughput(ctx context.Context, cancel context.CancelCauseFunc, read *atomic.Int64) (stop func()) {
	window, minBytes := p.downloadWatchdog()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(window)
		defer ticker.Stop()
		last := read.Load()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				n := read.Load()
				if n-last < minBytes {
					cancel(errImageSourceTooSlow)
					return
				}
				last = n
			}
		}
	}()
	return func() { close(done) }
}

// errForbiddenImageSource marks an image source address the provider refuses
// to connect to (loopback, link-local, unspecified, multicast).
var errForbiddenImageSource = stderrors.New("the image source address is not allowed")

// errTooManyImageRedirects marks an image source that redirects more than
// maxImageDownloadRedirects times.
var errTooManyImageRedirects = stderrors.New("the image source redirects too many times")

// maxImageDownloadBytesFromEnv returns the download limit in bytes from
// VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB (via getenv): the default
// (defaultMaxImageDownloadGiB) when it is unset, and — with a WARN log — when
// it is not an integer between minMaxImageDownloadGiB and
// maxMaxImageDownloadGiB.
func maxImageDownloadBytesFromEnv(getenv func(string) string, logger *slog.Logger) int64 {
	raw := strings.TrimSpace(getenv(maxImageDownloadGiBEnv))
	if raw == "" {
		return defaultMaxImageDownloadGiB * bytesPerGiB
	}
	gib, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || gib < minMaxImageDownloadGiB || gib > maxMaxImageDownloadGiB {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("Invalid image download limit; using the default",
			"env", maxImageDownloadGiBEnv, "value", raw,
			"min_gib", minMaxImageDownloadGiB, "max_gib", maxMaxImageDownloadGiB,
			"default_gib", defaultMaxImageDownloadGiB)
		return defaultMaxImageDownloadGiB * bytesPerGiB
	}
	return gib * bytesPerGiB
}

// imageDownloadLimit is the provider's download limit in bytes (the default
// for a Provider built without New).
func (p *Provider) imageDownloadLimit() int64 {
	if p.maxImageDownloadBytes > 0 {
		return p.maxImageDownloadBytes
	}
	return defaultMaxImageDownloadGiB * bytesPerGiB
}

// platformEndpoints are cloud metadata and platform endpoints outside the
// link-local range that an image download must never reach.
var platformEndpoints = []net.IP{
	net.ParseIP("fd00:ec2::254"),   // AWS instance metadata over IPv6
	net.ParseIP("100.100.100.200"), // Alibaba Cloud instance metadata
	net.ParseIP("168.63.129.16"),   // Azure WireServer
}

// nat64WellKnownPrefix is 64:ff9b::/96 (RFC 6052), whose last 32 bits are an
// IPv4 address that a NAT64 gateway connects to.
var nat64WellKnownPrefix = net.ParseIP("64:ff9b::").To16()[:12]

// embeddedIPv4 returns the IPv4 address an IPv6 address carries in its last 32
// bits when it is in the NAT64 well-known prefix (64:ff9b::/96) or is an
// IPv4-compatible address (::/96), or nil.
func embeddedIPv4(ip net.IP) net.IP {
	if ip.To4() != nil {
		return nil // already IPv4 (or IPv4-mapped, which To4 unwraps)
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}
	var zero [12]byte
	if bytes.Equal(ip16[:12], nat64WellKnownPrefix) || bytes.Equal(ip16[:12], zero[:]) {
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	}
	return nil
}

// forbiddenImageSourceIP reports whether ip is an address an image download
// must never connect to: loopback (unless allowLoopback, tests only),
// link-local unicast or multicast, any multicast, unspecified, in 0.0.0.0/8,
// a known cloud metadata or platform endpoint (platformEndpoints), or an IPv6
// address embedding such an IPv4 address (NAT64 64:ff9b::/96, IPv4-compatible
// ::/96).
func forbiddenImageSourceIP(ip net.IP, allowLoopback bool) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if v4 := embeddedIPv4(ip); v4 != nil && forbiddenImageSourceIP(v4, allowLoopback) {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	for _, endpoint := range platformEndpoints {
		if ip.Equal(endpoint) {
			return true
		}
	}
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

// imageProxyEnv names a proxy (an http:// or https:// URL) image downloads go
// through. Unset, they connect directly: HTTP_PROXY/HTTPS_PROXY are NOT
// honoured for image downloads — the vCenter SOAP client reads those, and a
// proxy resolves named targets itself, which would silently bypass the
// address checks for every image download.
const imageProxyEnv = "VIRTRIGAUD_VSPHERE_IMAGE_PROXY"

// imageProxyFromEnv returns the image-download proxy from
// VIRTRIGAUD_VSPHERE_IMAGE_PROXY (via getenv): nil when unset, and — with a
// WARN log — when it is not an http(s) URL with a host (downloads then connect
// directly).
func imageProxyFromEnv(getenv func(string) string, logger *slog.Logger) *url.URL {
	raw := strings.TrimSpace(getenv(imageProxyEnv))
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != ovaURLSchemeHTTP && u.Scheme != ovaURLSchemeHTTPS) || u.Host == "" {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("Invalid image download proxy; image downloads connect directly", "env", imageProxyEnv)
		return nil
	}
	return u
}

// checkImageSourceURL returns an error wrapping errForbiddenImageSource unless
// u is an http(s) URL whose host is not a forbidden IP literal or a localhost
// name. A named host is checked again, resolved, when the connection is made
// (imageDownloadClient).
func checkImageSourceURL(u *url.URL, allowLoopback bool) error {
	if u.Scheme != ovaURLSchemeHTTP && u.Scheme != ovaURLSchemeHTTPS {
		return fmt.Errorf("scheme %q: %w", u.Scheme, errForbiddenImageSource)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return fmt.Errorf("no host: %w", errForbiddenImageSource)
	}
	if (host == localhostName || strings.HasSuffix(host, localhostSuffix)) && !allowLoopback {
		return fmt.Errorf("host %q: %w", host, errForbiddenImageSource)
	}
	if ip := net.ParseIP(host); ip != nil && forbiddenImageSourceIP(ip, allowLoopback) {
		return fmt.Errorf("address %s: %w", ip, errForbiddenImageSource)
	}
	return nil
}

// imageDownloadClient returns the HTTP client for OVA downloads (see the
// comment at the top of this file). It connects directly unless proxy (from
// VIRTRIGAUD_VSPHERE_IMAGE_PROXY) is set; the process-wide HTTP(S)_PROXY is
// ignored. allowLoopback exists for tests, whose image servers listen on
// 127.0.0.1.
func imageDownloadClient(allowLoopback bool, proxy *url.URL) *http.Client {
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxy != nil {
		proxyFunc = http.ProxyURL(proxy)
	}
	dialer := &net.Dialer{
		Timeout:   imageDownloadDialTimeout,
		KeepAlive: 30 * time.Second,
		// Control runs on the resolved address of every connection, after DNS
		// and for every redirect target: a name cannot smuggle in a forbidden
		// address.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("address %q: %w", address, errForbiddenImageSource)
			}
			ip := net.ParseIP(host)
			if ip == nil || forbiddenImageSourceIP(ip, allowLoopback) {
				return fmt.Errorf("address %s: %w", host, errForbiddenImageSource)
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   imageDownloadTLSHandshakeTimeout,
		ResponseHeaderTimeout: imageDownloadResponseHeaderTimeout,
		IdleConnTimeout:       imageDownloadIdleConnTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxImageDownloadRedirects {
				return errTooManyImageRedirects
			}
			if len(via) > 0 && via[len(via)-1].URL.Scheme == ovaURLSchemeHTTPS && req.URL.Scheme != ovaURLSchemeHTTPS {
				// Never downgrade: an https source's content must not come over
				// plain http.
				return fmt.Errorf("redirect from https to %s: %w", req.URL.Scheme, errForbiddenImageSource)
			}
			return checkImageSourceURL(req.URL, allowLoopback)
		},
	}
}
