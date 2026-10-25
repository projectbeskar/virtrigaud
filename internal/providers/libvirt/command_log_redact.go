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
	"fmt"
	"regexp"
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
// every URL in such a text by a marker that names its scheme, except the libvirt
// connection URI (qemu:///system, qemu+ssh://user@host/system), which names
// the provider's own host and is what makes the log line useful, and a
// pvc:// location (a file on the provider's own migration volume). The command
// name and every other argument (domain names, pool paths, flags) are kept.
//
// S3 credentials never reach a host command line: the s3 client runs in the
// provider and streams through stdin, and an image download's URL reaches
// curl on stdin as well (image.go). Only the log text is redacted; the
// VirshError fields that classify a failure (ExitCode, Stderr) keep the
// original text.

// redactedURLFmt renders a redacted URL: its scheme only, without "://", so
// that no redacted text reads as a location.
const redactedURLFmt = "<redacted %s URL>"

// commandURLRE matches a URL in a command line or command output: a scheme,
// "://", and everything up to the next blank or quote (a remote command line
// single-quotes each argument, shellQuote).
var commandURLRE = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*)://[^\s'"]*`)

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
// (see the file comment).
func redactCommandText(text string) string {
	if !strings.Contains(text, "://") {
		return text
	}
	return commandURLRE.ReplaceAllStringFunc(text, func(m string) string {
		scheme, _, _ := strings.Cut(m, "://")
		if keepsURL(scheme) {
			return m
		}
		return fmt.Sprintf(redactedURLFmt, strings.ToLower(scheme))
	})
}
