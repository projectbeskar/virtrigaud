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

package signer

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/safefile"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
)

// maxKeyFileBytes bounds the key file.
const maxKeyFileBytes = 16 << 10

// SocketMode is the mode of the signer's socket: the signer's user and the
// directory's group (the agent's) may connect.
const SocketMode fs.FileMode = 0o660

// LoadKey reads the host key at path: a regular file, not reached through a
// symbolic link, owned by uid and with no permission for group or others, of
// at most 16 KiB, holding one ECDSA P-256 private key in PEM.
func LoadKey(path string, uid uint32) (*ecdsa.PrivateKey, error) {
	b, err := safefile.Read(path, safefile.Policy{Owner: &uid, DenyMode: 0o077, Max: maxKeyFileBytes})
	if err != nil {
		return nil, fmt.Errorf("read the host key: %w", err)
	}
	k, err := trust.ParseHostKey(b)
	if err != nil {
		return nil, fmt.Errorf("load the host key: %w", err)
	}
	return k, nil
}

// CheckSocketDir refuses a socket directory that is not a real directory
// owned by uid (the signer's user) or that grants anything to others, or
// write permission to its group: only the signer and the group the directory
// names (the agent's) can reach the socket, and the group cannot replace it.
func CheckSocketDir(dir string, uid uint32) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("socket directory %q is not a clean absolute path", dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat the socket directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (or is a symbolic link)", dir)
	}
	owner, ok := fileOwner(fi)
	if !ok || owner != uid {
		return fmt.Errorf("%s is not owned by uid %d", dir, uid)
	}
	if fi.Mode().Perm()&0o027 != 0 {
		return fmt.Errorf("%s has mode %04o; it must grant nothing to others and no write to its group", dir, fi.Mode().Perm())
	}
	return nil
}

// Listen listens on the Unix socket at path, after removing a stale socket
// there (and nothing else: a path that is not a socket is an error), and sets
// SocketMode on it. The socket is created under the process's umask, which
// should deny group and others, so it is never reachable before it has its
// mode.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat the socket path: %w", err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("set the socket mode: %w", err)
	}
	return lis, nil
}
