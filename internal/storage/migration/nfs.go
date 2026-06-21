/*
Copyright 2025.

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

package migration

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// nfsUIDGIDMax is the AUTH_SYS uid/gid ceiling (uint32). Values outside
// [0, nfsUIDGIDMax] are rejected so they cannot smuggle anything into the URL.
const nfsUIDGIDMax = 4294967295

// NFSURL builds the libnfs/qemu-img URL for an NFS staging object —
// "nfs://<server><export>/<path>/<key>[?uid=N&gid=N]" — from the NFS coordinates
// in opts and a relative object key.
//
// This is the single, hardened construction site for the NFS URL (ADR-0006
// security condition C7'). The provider hands the result to `qemu-img` on argv,
// so the inputs are sanitised here to prevent query-parameter smuggling
// (?/&/#), path traversal ("..") and control/whitespace injection, and uid/gid
// are range-checked. The server host is additionally SSRF-validated by the
// controller's HostPolicy before it ever reaches here.
func NFSURL(opts StorageOptions, key string) (string, error) {
	server := strings.TrimSpace(opts.Server)
	if server == "" {
		return "", fmt.Errorf("nfs server is empty")
	}
	if err := rejectURLUnsafe("nfs server", server, false); err != nil {
		return "", err
	}

	export := strings.TrimSpace(opts.Export)
	if export == "" || !strings.HasPrefix(export, "/") {
		return "", fmt.Errorf("nfs export must be an absolute path, got %q", export)
	}
	for _, seg := range []struct {
		name, val string
	}{
		{"nfs export", export},
		{"nfs path", opts.Path},
		{"nfs key", key},
	} {
		if err := rejectURLUnsafe(seg.name, seg.val, true); err != nil {
			return "", err
		}
	}

	// Join the export, the optional sub-path, and the key into one clean absolute
	// path. Because ".." segments are rejected above, Clean cannot escape the
	// export.
	full := path.Clean("/" +
		strings.Trim(export, "/") + "/" +
		strings.Trim(opts.Path, "/") + "/" +
		strings.Trim(key, "/"))

	u := "nfs://" + server + full

	q := url.Values{}
	if opts.UID != nil {
		if *opts.UID < 0 || *opts.UID > nfsUIDGIDMax {
			return "", fmt.Errorf("nfs uid %d out of range [0,%d]", *opts.UID, nfsUIDGIDMax)
		}
		q.Set("uid", strconv.FormatInt(*opts.UID, 10))
	}
	if opts.GID != nil {
		if *opts.GID < 0 || *opts.GID > nfsUIDGIDMax {
			return "", fmt.Errorf("nfs gid %d out of range [0,%d]", *opts.GID, nfsUIDGIDMax)
		}
		q.Set("gid", strconv.FormatInt(*opts.GID, 10))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u, nil
}

// rejectURLUnsafe rejects values that could break out of their position in the
// libnfs URL. It always rejects control characters, whitespace, and the URL
// query/fragment delimiters (?/#/&). For path components (isPath) it also
// rejects ".." segments; for the server host it additionally rejects '/'. An
// empty value is allowed (an absent sub-path or key).
func rejectURLUnsafe(name, v string, isPath bool) error {
	if v == "" {
		return nil
	}
	for _, r := range v {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("%s contains a control character", name)
		case r == ' ' || r == '\t':
			return fmt.Errorf("%s contains whitespace", name)
		case r == '?' || r == '#' || r == '&':
			return fmt.Errorf("%s contains a URL delimiter %q", name, r)
		case !isPath && r == '/':
			return fmt.Errorf("%s must not contain '/'", name)
		}
	}
	if isPath {
		for _, seg := range strings.Split(v, "/") {
			if seg == ".." {
				return fmt.Errorf("%s contains a '..' path segment", name)
			}
		}
	}
	return nil
}
