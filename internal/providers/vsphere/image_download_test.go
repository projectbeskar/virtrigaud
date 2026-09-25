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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestForbiddenImageSourceIP(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "127.9.9.9", "::1", // loopback
		"169.254.169.254", "169.254.0.1", "fe80::1", // link-local, cloud metadata
		"::ffff:169.254.169.254", "::ffff:127.0.0.1", // IPv4-mapped
		"0.0.0.0", "0.1.2.3", "::", // unspecified, 0.0.0.0/8
		"224.0.0.1", "239.1.1.1", "ff02::1", "ff01::1", // multicast
	} {
		assert.True(t, forbiddenImageSourceIP(net.ParseIP(s), false), "%s must be refused", s)
	}
	for _, s := range []string{
		"10.0.0.5", "172.16.1.1", "192.168.1.10", "fd00::1", // private: image servers are internal
		"8.8.8.8", "2001:db8::1",
	} {
		assert.False(t, forbiddenImageSourceIP(net.ParseIP(s), false), "%s must stay allowed", s)
	}
	assert.False(t, forbiddenImageSourceIP(net.ParseIP("127.0.0.1"), true), "tests may allow loopback")
	assert.True(t, forbiddenImageSourceIP(net.ParseIP("169.254.169.254"), true), "never link-local")
}

func TestCheckImageSourceURL(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/x.ova", "http://[::1]:8080/x.ova", "http://169.254.169.254/latest/meta-data/",
		"http://localhost/x.ova", "http://LOCALHOST./x.ova", "http://images.localhost/x.ova",
		"http://0.0.0.0/x.ova", "ftp://images.example.com/x.ova", "file:///etc/passwd", "http:///x.ova",
	} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.ErrorIs(t, checkImageSourceURL(u, false), errForbiddenImageSource, raw)
	}
	for _, raw := range []string{"https://images.example.com/x.ova", "http://10.1.2.3:8080/x.ova", "https://[fd00::1]/x.ova"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.NoError(t, checkImageSourceURL(u, false), raw)
	}
}

func TestImageDownloadClient_DialRefusesForbiddenAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer srv.Close()

	// No pre-check here: the dial hook alone refuses the resolved loopback
	// address (what a hostname resolving to 127.0.0.1 would reach).
	_, err := imageDownloadClient(false).Get(srv.URL)
	assert.ErrorIs(t, err, errForbiddenImageSource)

	resp, err := imageDownloadClient(true).Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	tr, ok := imageDownloadClient(false).Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, imageDownloadTLSHandshakeTimeout, tr.TLSHandshakeTimeout)
	assert.Equal(t, imageDownloadResponseHeaderTimeout, tr.ResponseHeaderTimeout)
	assert.NotNil(t, tr.DialContext)
}

// downloadProvider is a Provider that downloads without vCenter.
func downloadProvider(allowLoopback bool, maxBytes int64) *Provider {
	return &Provider{
		logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowLoopbackImageSources: allowLoopback,
		maxImageDownloadBytes:     maxBytes,
	}
}

func TestDownloadOVA_RefusesForbiddenSources(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ova"))
	}))
	defer srv.Close()

	p := downloadProvider(false, 0)
	for _, u := range []string{
		srv.URL + "/image.ova",
		strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/image.ova",
		"http://169.254.169.254/latest/meta-data/iam/image.ova",
	} {
		_, cleanup, err := p.downloadOVA(context.Background(), u)
		cleanup()
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "address is not allowed")
	}
	assert.Zero(t, hits.Load(), "no request reached the loopback server")
}

func TestDownloadOVA_Redirects(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n == 0 {
			_, _ = w.Write([]byte("ova"))
			return
		}
		http.Redirect(w, r, srv.URL+"/hop/"+strconv.Itoa(n-1), http.StatusFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://images.example.com/x.ova", http.StatusFound)
	})
	p := downloadProvider(true, 0) // the test server is on loopback

	path, cleanup, err := p.downloadOVA(context.Background(), srv.URL+"/hop/3")
	require.NoError(t, err, "three redirects are followed")
	assert.NotEmpty(t, path)
	cleanup()

	hits.Store(0)
	_, cleanup, err = p.downloadOVA(context.Background(), srv.URL+"/hop/4")
	cleanup()
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "redirects more than 3 times")
	assert.Equal(t, int32(4), hits.Load(), "the fourth redirect is not followed")

	for _, u := range []string{srv.URL + "/to-metadata", srv.URL + "/to-ftp"} {
		_, cleanup, err = p.downloadOVA(context.Background(), u)
		cleanup()
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "address is not allowed", u)
	}
}

func TestDownloadOVA_SizeLimit(t *testing.T) {
	const limit = 1024
	body := bytes.Repeat([]byte("a"), limit+1)
	mux := http.NewServeMux()
	mux.HandleFunc("/sized.ova", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/chunked.ova", func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: the limit applies while reading.
		flusher, ok := w.(http.Flusher)
		for _, b := range body {
			_, _ = w.Write([]byte{b})
			if ok {
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/fits.ova", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body[:limit]) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := downloadProvider(true, limit)

	for _, u := range []string{srv.URL + "/sized.ova", srv.URL + "/chunked.ova"} {
		_, cleanup, err := p.downloadOVA(context.Background(), u)
		cleanup()
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "larger than this provider's download limit", u)
	}
	path, cleanup, err := p.downloadOVA(context.Background(), srv.URL+"/fits.ova")
	require.NoError(t, err)
	defer cleanup()
	assert.NotEmpty(t, path)
}

func TestDownloadOVA_MessagesCarryNoTransportDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret internal page", http.StatusTeapot)
	}))
	defer srv.Close()
	p := downloadProvider(true, 0)

	_, cleanup, err := p.downloadOVA(context.Background(), srv.URL+"/image.ova")
	cleanup()
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "HTTP client error")
	assert.NotContains(t, err.Error(), "418")
	assert.NotContains(t, err.Error(), "secret internal page")
	assert.NotContains(t, err.Error(), "127.0.0.1")

	_, cleanup, err = p.downloadOVA(context.Background(), "http://127.0.0.1:1/image.ova")
	cleanup()
	requireCode(t, err, codes.Unavailable)
	assert.NotContains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.Error(), "127.0.0.1")
}

func TestMaxImageDownloadBytesFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == maxImageDownloadGiBEnv {
				return v
			}
			return ""
		}
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	assert.Equal(t, int64(256)*bytesPerGiB, maxImageDownloadBytesFromEnv(env(""), logger))
	assert.Equal(t, int64(10)*bytesPerGiB, maxImageDownloadBytesFromEnv(env("10"), logger))
	assert.Equal(t, int64(1)*bytesPerGiB, maxImageDownloadBytesFromEnv(env("1"), logger))
	assert.Equal(t, int64(16384)*bytesPerGiB, maxImageDownloadBytesFromEnv(env(" 16384 "), logger))
	assert.Empty(t, logs.String())

	for _, bad := range []string{"0", "16385", "-1", "ten", "1.5"} {
		logs.Reset()
		assert.Equal(t, int64(256)*bytesPerGiB, maxImageDownloadBytesFromEnv(env(bad), logger), bad)
		assert.Contains(t, logs.String(), "level=WARN", bad)
		assert.Contains(t, logs.String(), maxImageDownloadGiBEnv)
	}
	assert.Equal(t, int64(256)*bytesPerGiB, (&Provider{}).imageDownloadLimit(), "a Provider built without New")
}
