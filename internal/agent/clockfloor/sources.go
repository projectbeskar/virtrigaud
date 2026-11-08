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
