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
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Redaction of host command lines in the provider log (R28).
//
// The shared transport logs every command it runs (DEBUG) and every command
// that fails (ERROR, logCommandFailure), and a failed command's VirshError
// carries the command line and its output into the error chains callers log.
// Some commands carry a location that is not the provider's to publish: the
// nfs:// URL of a migration's staged disk (the tenant's NFS server, export,
// path and its uid/gid options) that `qemu-img convert` reads or writes, and
// qemu-img echoes that URL in its error text. redactCommandText replaces
// every URL in such a text by a marker that names its scheme, except the
// libvirt connection URI (qemu:///system, qemu+ssh://user@host/system), which
// names the provider's own host and is what makes the log line useful, and a
// pvc:// location (a file on the provider's own migration volume). The
// command name and every other argument (domain names, pool paths, flags) are
// kept.
//
// libnfs (through qemu-img) also names a staged object without a scheme — its
// server, export and path in a lookup or mount error. A call that works on a
// staged nfs object marks its context with those texts
// (withNFSLogRedactions), and the transport's log lines and the VirshErrors it
// returns replace them as well (redactForLog).
//
// S3 credentials never reach a host command line: the s3 client runs in the
// provider and streams through stdin, and ImagePrepare's download URL reaches
// curl on stdin as well (image.go). The legacy create-from-URL path
// (storage.go, DownloadCloudImage) still passes its URL to wget as an
// argument: it is redacted in the log, but visible in the host's process
// list. Only log text is redacted; the VirshError fields that classify a
// failure (ExitCode, Stderr) keep the original text.

// redactedURLFmt renders a redacted URL: its scheme only, without "://", so
// that no redacted text reads as a location.
const redactedURLFmt = "<redacted %s URL>"

// commandURLRE matches a URL in a command line or command output: a scheme,
// "://", and everything up to the next blank or quote (a remote command line
// single-quotes each argument, shellQuote).
var commandURLRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*)://[^\s'"]*`)

// keepsURL reports whether a URL of scheme stays readable in the log: a
// libvirt connection URI (qemu, qemu+ssh, qemu+tls, ...), which names the
// provider's own host and carries no credential, and a pvc:// location, a
// file on the provider's own migration volume.
func keepsURL(scheme string) bool {
	s := strings.ToLower(scheme)
	return s == "qemu" || strings.HasPrefix(s, "qemu+") || s+"://" == pvcURLScheme
}

// redactCommandText returns text with every URL in it, other than a libvirt
// connection URI and a pvc:// location, replaced by "<redacted <scheme> URL>"
// (see the file comment). A kept URL is kept only up to the next URL glued to
// it ("qemu:///system,nfs://..."): the rest of the match is redacted again.
func redactCommandText(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	return commandURLRE.ReplaceAllStringFunc(text, func(m string) string {
		scheme, rest, _ := strings.Cut(m, "://")
		if keepsURL(scheme) {
			return scheme + "://" + redactCommandText(rest)
		}
		return fmt.Sprintf(redactedURLFmt, strings.ToLower(scheme))
	})
}

// redactImageSpec renders an image reference for the log: a URL with its
// user-info and query dropped (redactURL), anything else (a path, an image
// name) as it is.
func redactImageSpec(spec string) string {
	if strings.Contains(spec, "://") {
		return redactURL(spec)
	}
	return spec
}

// logRedactionsKey marks a context with the literal texts its commands' log
// lines must not show (withLogRedactions).
type logRedactionsKey struct{}

// minLogRedactionLen is the shortest text withLogRedactions takes: a shorter
// one ("/", a one-letter host) would mangle every log line it appears in.
const minLogRedactionLen = 4

// withLogRedactions returns ctx marked so that the commands run under it log,
// and render in their errors, every one of locations (and those ctx already
// carries) replaced by stagedLocationMarker. Texts shorter than
// minLogRedactionLen are ignored. The longest texts are replaced first, so a
// path is replaced before the directory it is in.
func withLogRedactions(ctx context.Context, locations ...string) context.Context {
	next := slices.Clone(logRedactionsOf(ctx))
	for _, l := range locations {
		if l = strings.TrimSpace(l); len(l) >= minLogRedactionLen && !slices.Contains(next, l) {
			next = append(next, l)
		}
	}
	slices.SortStableFunc(next, func(a, b string) int { return len(b) - len(a) })
	return context.WithValue(ctx, logRedactionsKey{}, next)
}

// logRedactionsOf returns the texts ctx was marked with (withLogRedactions).
func logRedactionsOf(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(logRedactionsKey{}).([]string)
	return v
}

// nfsURLLocations returns the texts that locate a staged nfs object, for
// withLogRedactions and redactLocations: the URL itself, its path, the
// directory and file name in it, and its server. libnfs names them without
// the scheme.
func nfsURLLocations(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	return []string{raw, u.Path, path.Dir(u.Path), path.Base(u.Path), u.Hostname()}
}

// withNFSLogRedactions is withLogRedactions with the locations of the staged
// nfs object nfsURL.
func withNFSLogRedactions(ctx context.Context, nfsURL string) context.Context {
	return withLogRedactions(ctx, nfsURLLocations(nfsURL)...)
}

// redactForLog is redactCommandText followed by the literal redactions ctx
// carries (withLogRedactions).
func redactForLog(ctx context.Context, text string) string {
	return redactText(redactCommandText(text), logRedactionsOf(ctx)...)
}
