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

package libvirt

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// secretImageURL is an image URL with user-info and a presigned query: none of
// legacyURLSecrets may reach a host command line, the log or an error.
const secretImageURL = "https://joe:p4ssw0rd@images.example.com/ubuntu.qcow2?X-Amz-Signature=supersecret&token=t0ken"

// legacyURLSecrets are the parts of secretImageURL that must not leak.
var legacyURLSecrets = []string{"joe", "p4ssw0rd", "supersecret", "t0ken", "X-Amz-Signature"}

// captureStdLog sends the standard logger (the provider's log) to a buffer
// for the rest of the test.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// assertNoURLSecret fails when text carries a part of secretImageURL that
// must not leak.
func assertNoURLSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range legacyURLSecrets {
		assert.NotContains(t, text, s, "%s carries a secret part of the image URL", where)
	}
}

// TestCreate_URLImageNeverOnCommandLine: Create's legacy create-from-URL path
// hands the URL to curl on stdin, never as an argument. The command lines run
// on the host (and so its process list and audit log) carry neither the
// user-info nor the query, the provider log shows the URL redacted, and the
// image is still downloaded and converted.
func TestCreate_URLImageNeverOnCommandLine(t *testing.T) {
	c := newCreateHost(t)
	logs := captureStdLog(t)
	req := c.createReq(ownerTeamA, "")
	req.Image = contracts.VMImage{URL: secretImageURL}

	resp, err := c.p.Create(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	assert.FileExists(t, filepath.Join(c.images, "team-a.web-disk.qcow2"), "the image was downloaded and converted")

	curlArgs := c.log("curl")
	assert.True(t, strings.HasPrefix(curlArgs, "-q -K - --globoff "), "curl argv: %s", curlArgs)
	assert.Contains(t, curlArgs, "--proto =http,https,ftp --proto-redir =http,https,ftp")
	assert.Contains(t, curlArgs, "--connect-timeout 30 --max-time 1800 --max-filesize 274877906944")
	assert.Equal(t, "globoff\n"+`url = "`+secretImageURL+`"`+"\n", c.log("curlrc"), "the URL reaches curl on stdin only")
	assert.Empty(t, c.log("wget"), "wget is never run")
	for _, tool := range []string{"curl", "virsh", "qemu-img", "sudo", "genisoimage"} {
		assertNoURLSecret(t, tool+" command line", c.log(tool))
		assert.NotContains(t, c.log(tool), "images.example.com", "%s command line names the image host", tool)
	}
	assertNoURLSecret(t, "the provider log", logs.String())
	assert.Contains(t, logs.String(), "https://images.example.com/ubuntu.qcow2?…", "the URL is logged redacted")
}

// TestCreate_URLImageDownloadFailureLeaksNothing: a failed download is an
// error that does not echo the URL, and curl's error text is logged with the
// URL redacted.
func TestCreate_URLImageDownloadFailureLeaksNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		rc        string
		retryable bool
	}{
		"not found (permanent)":       {rc: "78", retryable: false},
		"connection failed (retried)": {rc: "7", retryable: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCreateHost(t)
			logs := captureStdLog(t)
			plantFile(t, filepath.Join(c.root, "download.fail"), tc.rc+"\n")
			req := c.createReq(ownerTeamA, "")
			req.Image = contracts.VMImage{URL: secretImageURL}

			_, err := c.p.Create(context.Background(), req)
			require.Error(t, err)
			assertNoURLSecret(t, "the error", err.Error())
			assert.NotContains(t, err.Error(), "images.example.com")
			assert.Equal(t, tc.retryable, contracts.IsRetryable(err), "%v", err)
			assertNoURLSecret(t, "the provider log", logs.String())
			assert.Contains(t, logs.String(), "simulated failure", "curl's error is logged")
			assertNoURLSecret(t, "the curl command line", c.log("curl"))
			for _, e := range c.stagingEntries() {
				assert.NotContains(t, e, "-disk-temp.img.", "the failed download's staging file is removed")
			}
		})
	}
}

// TestDownloadCloudImage_RefusesUnsafeURL: a URL curl's config syntax could
// be broken out of (control characters, a quote is escaped) or that is not
// http(s)/ftp is refused before anything runs on the host, without echoing
// it.
func TestDownloadCloudImage_RefusesUnsafeURL(t *testing.T) {
	h := newFakeHost(t)
	sp := NewStorageProvider(h.host("h1"))
	sp.stagingDir = t.TempDir()
	for _, bad := range []string{
		"https://images.example.com/a.qcow2\noutput = /etc/passwd",
		"https://images.example.com/a b.qcow2?token=t0ken",
		"file:///etc/shadow",
	} {
		_, err := sp.DownloadCloudImage(context.Background(), bad, "vol-disk", "default", 10)
		require.Error(t, err, bad)
		var pe *contracts.ProviderError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, contracts.ErrorTypeInvalidSpec, pe.Type)
		assert.NotContains(t, err.Error(), "images.example.com")
		assert.NotContains(t, err.Error(), "t0ken")
	}
	assert.Empty(t, h.log("curl"), "nothing is downloaded")
}
