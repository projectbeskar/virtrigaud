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
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// CheckPeer refuses a peer whose uid is not want.
func CheckPeer(got, want uint32) error {
	if got != want {
		return fmt.Errorf("%w: uid %d is not uid %d", ErrPeer, got, want)
	}
	return nil
}

// ResolveUID turns a user name or a decimal uid into a uid. It reads the
// local user database only.
func ResolveUID(spec string) (uint32, error) {
	if n, err := strconv.ParseUint(spec, 10, 32); err == nil {
		return uint32(n), nil
	}
	u, err := user.Lookup(spec)
	if err != nil {
		return 0, fmt.Errorf("resolve user %q: %w", spec, err)
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("resolve user %q: uid %q is not a number", spec, u.Uid)
	}
	return uint32(n), nil
}

// EUID returns the process's effective uid.
func EUID() uint32 { return uint32(os.Geteuid()) } // #nosec G115 -- a uid is 32 bits
