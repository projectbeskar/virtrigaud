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

package inventory

import "golang.org/x/sys/unix"

// openFlags are the open(2) flags for reading data: read-only, never a
// controlling terminal, never blocking on a FIFO or device, and without
// following a symbolic link unless asked.
func openFlags(follow bool) int {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOCTTY
	if !follow {
		flags |= unix.O_NOFOLLOW
	}
	return flags
}
