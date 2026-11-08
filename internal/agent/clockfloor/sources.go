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

package clockfloor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// BootClock reads CLOCK_BOOTTIME: monotonic time since boot that keeps
// counting while the system is suspended and that a wall-clock change does not
// move.
type BootClock interface {
	// BootTime returns the time since boot.
	BootTime() (time.Duration, error)
}

// BootIDSource reads the kernel's boot_id, a value that differs on every
// boot.
type BootIDSource interface {
	// BootID returns the identifier of the current boot.
	BootID() (string, error)
}

// TimeNamespaceCheck verifies that the process runs in the host's initial
// time namespace, or in one with no clock offset.
type TimeNamespaceCheck interface {
	// CheckTimeNamespace returns nil when no clock offset applies, and an
	// error (ErrTimeNamespace for a non-zero offset) otherwise.
	CheckTimeNamespace() error
}

// DefaultTimeNSOffsetsPath is the file Linux publishes a process's time
// namespace offsets in (kernels with time namespaces).
const DefaultTimeNSOffsetsPath = "/proc/self/timens_offsets"

// ProcTimeNS refuses a time namespace with a non-zero monotonic or boottime
// offset. A process can read its own offsets without privilege, which is why
// this reads them rather than comparing the namespace links with those of
// the init process (which needs privilege). A namespace whose offsets are all
// zero is harmless and accepted; a kernel without the file has no time
// namespaces.
type ProcTimeNS struct {
	// Path is the offsets file; empty means DefaultTimeNSOffsetsPath.
	Path string
}

// CheckTimeNamespace reads and parses the offsets file.
func (p ProcTimeNS) CheckTimeNamespace() error {
	path := p.Path
	if path == "" {
		path = DefaultTimeNSOffsetsPath
	}
	b, err := os.ReadFile(path) // #nosec G304 -- a fixed kernel file or a test fixture
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read time namespace offsets: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return errors.New("read time namespace offsets: malformed line")
		}
		for _, v := range f[1:] {
			if v != "0" {
				return fmt.Errorf("%w: %s", ErrTimeNamespace, f[0])
			}
		}
	}
	return nil
}

// DefaultBootIDPath is where Linux publishes the boot_id.
const DefaultBootIDPath = "/proc/sys/kernel/random/boot_id"

// ProcBootID reads the boot_id from a file, DefaultBootIDPath by default. The
// path is a field so a test can point it at a temporary file.
type ProcBootID struct {
	// Path is the file to read; empty means DefaultBootIDPath.
	Path string
}

// BootID reads, trims and validates the boot_id. A missing, empty, too long
// or non-hexadecimal value is an error.
func (p ProcBootID) BootID() (string, error) {
	path := p.Path
	if path == "" {
		path = DefaultBootIDPath
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the path is a fixed kernel file or a test fixture
	if err != nil {
		return "", fmt.Errorf("read boot_id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" || len(id) > maxBootIDLen {
		return "", errors.New("read boot_id: empty or too long")
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdefABCDEF-", r) {
			return "", errors.New("read boot_id: unexpected character")
		}
	}
	return id, nil
}
