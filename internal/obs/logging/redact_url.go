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

package logging

import (
	"net/url"
	"strings"
)

// RedactURL renders raw for a log line: its scheme, host and path only. The
// user-info, query and fragment are dropped (they may carry credentials or a
// presigned token); "?…" marks a dropped query. "" stays "". A value
// without "://", or one url.Parse refuses, is replaced by a marker and never
// echoed: it is not a URL this can take apart, and it may still carry a
// credential.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "<not a URL>"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable URL>"
	}
	out := u.Scheme + "://" + u.Host + u.EscapedPath()
	if u.RawQuery != "" || u.ForceQuery {
		out += "?…"
	}
	return out
}

// redactedLocation is what RedactLocation renders in place of a location.
const redactedLocation = "<redacted>"

// RedactLocation renders a migration staging location (an nfs://, s3:// or
// pvc:// URL) for a log line: its scheme only, "nfs://<redacted>". Unlike
// RedactURL it keeps neither host nor path: an NFS server and export path, an
// S3 bucket and key, and the uid/gid query of an nfs URL name the tenant's
// staging storage (R28). "" stays ""; a value without a scheme is
// "<redacted>".
func RedactLocation(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	scheme, _, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" || strings.ContainsAny(scheme, " /?#@") {
		return redactedLocation
	}
	return strings.ToLower(scheme) + "://" + redactedLocation
}
