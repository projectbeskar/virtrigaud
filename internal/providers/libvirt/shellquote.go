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
	"errors"
	"regexp"
	"strings"
)

// Shell quoting for commands run on the hypervisor host over SSH.
//
// An SSH "exec" request carries ONE command string, which the remote sshd hands
// to the login user's shell (`$SHELL -c "<string>"`). Every argv-shaped call in
// this provider — virsh control commands, the "!" host-shell escape
// (runVirshCommand / hostconn.Conn.RunHost), and Stream / StreamIn — therefore
// has to be flattened into a string the remote shell will split back into
// EXACTLY the same argv. Joining with spaces (what this package used to do) lets
// the remote shell interpret any metacharacter in a caller-supplied value
// (domain names, snapshot descriptions, image URLs, disk paths, the libvirt URI
// path derived from a Host endpoint): `;`, `$(...)`, backticks, `|`, `&`,
// redirects, globs, and word-splitting on spaces. shellJoin quotes every
// element so the remote argv is byte-for-byte the local one — the SSH transport
// now has the same argv semantics as the local exec.Command path (runLocal).
//
// A caller that genuinely needs shell features (a redirect, a pipe) must say so
// explicitly with an argv such as {"sh", "-c", "<fixed script>", "sh", value...}
// where every variable input is passed as a positional parameter ("$1"), never
// interpolated into the script text (see writeStdinToFileArgv).

// errNULInShellArg is returned when an argument contains a NUL byte: it cannot
// be represented in a C-string command line (the remote shell would silently
// truncate it), so it is rejected rather than quoted.
var errNULInShellArg = errors.New("libvirt: command argument contains a NUL byte")

// shellSafeArgRE matches an argument that needs no quoting for a POSIX shell
// (and bash/zsh): only characters that are never special in an argument
// position. A leading '=' is excluded because zsh expands `=cmd` to a path.
// Anything else — including the empty string — is single-quoted.
var shellSafeArgRE = regexp.MustCompile(`^[A-Za-z0-9_@%+:,./-][A-Za-z0-9_@%+=:,./-]*$`)

// shellQuote single-quotes s for safe interpolation into a remote shell command
// line. Each embedded single quote is emitted as: close the quoted string, a
// backslash-escaped quote, reopen the quoted string. The result is always one
// shell word whose value is exactly s, whatever s contains (spaces, newlines,
// $, backticks, ;, |, quotes, ...).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellQuoteArg quotes s only when it needs it: a value made solely of
// shell-inert characters (shellSafeArgRE) is emitted verbatim so logged command
// lines stay readable (`virsh -c qemu:///system list --all`), everything else is
// shellQuote-d. Either way the remote shell yields exactly one word equal to s.
func shellQuoteArg(s string) string {
	if shellSafeArgRE.MatchString(s) {
		return s
	}
	return shellQuote(s)
}

// shellJoin flattens argv into a single command line for a remote POSIX shell,
// quoting every element (shellQuoteArg) so the shell reconstructs exactly argv:
// no word splitting, globbing, expansion, command substitution, or operator
// interpretation of any element. It rejects an element containing a NUL byte.
func shellJoin(argv []string) (string, error) {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if strings.IndexByte(a, 0) >= 0 {
			return "", errNULInShellArg
		}
		quoted[i] = shellQuoteArg(a)
	}
	return strings.Join(quoted, " "), nil
}

// writeStdinToFileScript is the fixed `sh -c` script behind
// writeStdinToFileArgv. The destination path is ALWAYS the positional parameter
// "$1" — never interpolated into this text — so no path can inject shell syntax.
const writeStdinToFileScript = `cat > "$1"`

// writePrivateFileScript is writeStdinToFileScript with a restrictive umask, for
// small files that may carry sensitive material (cloud-init user-data, domain
// XML): a newly created file is readable by the SSH user only, not world
// readable in a shared /tmp. Like writeStdinToFileScript, the path is "$1".
const writePrivateFileScript = `umask 077 && cat > "$1"`

// writeStdinToFileArgv returns the argv that copies the command's stdin to path
// on the host: `sh -c 'cat > "$1"' sh <path>`. It is the argv-safe replacement
// for a hand-built `cat > <path>` string; the redirect lives in a fixed script
// and path is passed as a positional parameter.
func writeStdinToFileArgv(path string) []string {
	return []string{"sh", "-c", writeStdinToFileScript, "sh", path}
}

// writePrivateFileArgv is writeStdinToFileArgv with umask 077 (see
// writePrivateFileScript).
func writePrivateFileArgv(path string) []string {
	return []string{"sh", "-c", writePrivateFileScript, "sh", path}
}
