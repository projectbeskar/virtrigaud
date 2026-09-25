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
	stderrors "errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
//     an http(s) scheme and an allowed address;
//   - bounds the TCP connect, TLS handshake and response-header waits (the
//     body itself may take long: images are large);
//   - reads at most the provider's download limit
//     (VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB).
//
// With an HTTP(S) proxy configured (HTTP_PROXY/HTTPS_PROXY), the provider
// connects to the proxy, which resolves a named target itself: restrict the
// proxy's egress too. An IP-literal or localhost target is refused before any
// request either way.

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

// forbiddenImageSourceIP reports whether ip is an address an image download
// must never connect to: loopback (unless allowLoopback, tests only),
// link-local unicast or multicast, any multicast, unspecified or in 0.0.0.0/8.
func forbiddenImageSourceIP(ip net.IP, allowLoopback bool) bool {
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
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
// comment at the top of this file). allowLoopback exists for tests, whose
// image servers listen on 127.0.0.1.
func imageDownloadClient(allowLoopback bool) *http.Client {
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
		Proxy:                 http.ProxyFromEnvironment,
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
			return checkImageSourceURL(req.URL, allowLoopback)
		},
	}
}
