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

package storage

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Redaction of staging locations in error texts (R28).
//
// A migration's staged object lives in the tenant's storage: an S3 bucket at
// an endpoint, or a file on an NFS server's export. Its bucket, key, endpoint,
// server and export path name the tenant's storage, and belong in no log line,
// status or event. The S3 client's errors name them (minio-go's transport
// errors quote the request URL, which carries the endpoint, bucket and key),
// and so does qemu-img's stderr for an nfs:// object. RedactLocations and
// RedactError remove them from such a text before it can reach a log line.

// StagedLocationMarker replaces a staged object's location in a redacted text.
const StagedLocationMarker = "<staged object>"

// redactedURLFormat renders a redacted URL: its scheme only, without "://", so
// that no redacted text reads as a location.
const redactedURLFormat = "<redacted %s URL>"

// MinRedactedLocationLen is the shortest location text RedactLocations
// replaces: a shorter one ("/", a one-letter name) would mangle every text it
// appears in. It is the shortest S3 bucket name.
const MinRedactedLocationLen = 3

// textURLRE matches a URL in free text: a scheme, "://", an optional
// bracketed IPv6 host, and everything up to the next blank, quote or bracket.
var textURLRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*)://(\[[0-9A-Fa-f:.%]+\])?[^\s'"<>()\[\]]*`)

// RedactLocations returns text with every URL in it replaced by
// "<redacted <scheme> URL>", then every one of locations (the longest first;
// texts shorter than MinRedactedLocationLen are ignored) replaced by
// StagedLocationMarker.
func RedactLocations(text string, locations ...string) string {
	if strings.Contains(text, "://") {
		text = textURLRE.ReplaceAllStringFunc(text, func(m string) string {
			scheme, _, _ := strings.Cut(m, "://")
			return fmt.Sprintf(redactedURLFormat, strings.ToLower(scheme))
		})
	}
	ls := make([]string, 0, len(locations))
	for _, l := range locations {
		if l = strings.TrimSpace(l); len(l) >= MinRedactedLocationLen && !slices.Contains(ls, l) {
			ls = append(ls, l)
		}
	}
	slices.SortStableFunc(ls, func(a, b string) int { return len(b) - len(a) })
	for _, l := range ls {
		text = strings.ReplaceAll(text, l, StagedLocationMarker)
	}
	return text
}

// locationRedactedError is an error whose text has its locations redacted
// (RedactError); its chain is the original error's, so classification by
// errors.As / errors.Is is unaffected.
type locationRedactedError struct {
	text string
	err  error
}

// Error returns the redacted text.
func (e *locationRedactedError) Error() string { return e.text }

// Unwrap returns the original error.
func (e *locationRedactedError) Unwrap() error { return e.err }

// RedactError returns err with its text redacted by RedactLocations (nil stays
// nil). The returned error unwraps to err.
func RedactError(err error, locations ...string) error {
	if err == nil {
		return nil
	}
	return &locationRedactedError{text: RedactLocations(err.Error(), locations...), err: err}
}

// S3EndpointLocations returns the texts that locate an S3 endpoint in an
// error: the endpoint as configured and its host[:port] and host (minio-go's
// dial errors name the host and port without a scheme).
func S3EndpointLocations(endpoint string) []string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	out := []string{endpoint}
	hostPort := endpoint
	if _, rest, ok := strings.Cut(endpoint, "://"); ok {
		hostPort = rest
	}
	hostPort, _, _ = strings.Cut(hostPort, "/")
	out = append(out, hostPort)
	if host, _, ok := strings.Cut(hostPort, ":"); ok && !strings.Contains(hostPort, "]") {
		out = append(out, host)
	}
	return out
}
