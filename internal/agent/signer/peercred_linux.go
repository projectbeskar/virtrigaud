//go:build linux

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
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// errNotUnix is returned for a connection that is not a Unix socket.
var errNotUnix = errors.New("not a Unix socket connection")

// PeerUID returns the effective uid of the process that created the other end
// of a Unix socket connection, as the kernel recorded it when the connection
// was made (SO_PEERCRED). It cannot be forged by the peer.
func PeerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errNotUnix
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) // #nosec G115 -- a file descriptor fits an int
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if serr != nil {
		return 0, fmt.Errorf("peer credentials: %w", serr)
	}
	return cred.Uid, nil
}
