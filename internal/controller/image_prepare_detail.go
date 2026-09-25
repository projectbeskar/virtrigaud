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

package controller

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxProviderDetailBytes caps the provider-supplied detail that image
// preparation copies into VMImage status, events and VM conditions. A VMImage
// can be shared with other namespaces, so its status and events are read by
// more than the tenant whose prepare produced them.
const maxProviderDetailBytes = 256

// providerDetailEllipsis ends a detail cut to maxProviderDetailBytes.
const providerDetailEllipsis = "…"

// sanitizeProviderDetail returns the message of a provider error err (as
// providerErrorMessage renders it) made safe to copy into status, events and
// conditions: the userinfo of every URL in it is removed (it may be a
// credential), control characters (newlines included) and invalid UTF-8
// become spaces, runs of whitespace collapse to one space, and the result is
// cut to maxProviderDetailBytes on a rune boundary.
//
// The messages of the image-prepare paths are written by the manager; the
// provider's detail is appended to them, never used alone. Providers already
// keep that detail to the requester's own objects (ADR-0009 D11: the owner of
// a conflicting artifact goes to the provider log only); this is defence in
// depth against a detail that is unexpectedly long, multi-line or carries a
// credential.
func sanitizeProviderDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := stripURLUserinfoInText(providerErrorMessage(err))
	msg = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, msg)
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) <= maxProviderDetailBytes {
		return msg
	}
	cut := maxProviderDetailBytes - len(providerDetailEllipsis)
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + providerDetailEllipsis
}

// stripURLUserinfoInText removes the userinfo ("user:password@") of every
// URL in free text: after each "://" it drops everything up to and including
// the last '@' that comes before the end of the authority (the first '/',
// '?', '#' or whitespace).
func stripURLUserinfoInText(text string) string {
	var b strings.Builder
	rest := text
	for {
		i := strings.Index(rest, "://")
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i+len("://")])
		rest = rest[i+len("://"):]
		end := strings.IndexFunc(rest, func(r rune) bool {
			return r == '/' || r == '?' || r == '#' || unicode.IsSpace(r)
		})
		authority := rest
		if end >= 0 {
			authority = rest[:end]
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			rest = rest[at+1:]
		}
	}
}
