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
	"log"
	"time"
)

// Best-effort probes. Describe enriches every poll with statistics and guest
// information that are routinely unavailable — no guest agent configured
// (domifaddr --source agent, guestinfo, guest-ping), a shut-off domain
// (dommemstat, cpu-stats, domblkstat) — and whose failure it ignores. Such a
// failure is expected, so it is logged at DEBUG; every other failed command
// keeps its ERROR lines.

// expectedFailuresKey marks a context whose commands are best-effort probes.
type expectedFailuresKey struct{}

// withExpectedFailures returns ctx marked so that a command run under it that
// fails is logged at DEBUG, not ERROR: its caller treats the failure as an
// expected answer (a probe), not as an error.
func withExpectedFailures(ctx context.Context) context.Context {
	return context.WithValue(ctx, expectedFailuresKey{}, true)
}

// failuresExpected reports whether ctx was marked by withExpectedFailures.
func failuresExpected(ctx context.Context) bool {
	v, ok := ctx.Value(expectedFailuresKey{}).(bool)
	return ok && v
}

// logCommandFailure logs a failed host or virsh command: at ERROR, or at
// DEBUG for a best-effort probe (withExpectedFailures). The command line and
// its stderr are logged with every URL but the libvirt connection URI
// redacted (redactCommandText, R28): a staged disk's nfs:// URL never reaches
// the log.
func logCommandFailure(ctx context.Context, command string, exitCode int, duration time.Duration, stderr string) {
	command, stderr = redactForLog(ctx, command), redactForLog(ctx, stderr)
	if failuresExpected(ctx) {
		log.Printf("DEBUG Probe command failed (expected; ignored by its caller): %s (exit code: %d, duration: %v): %s",
			command, exitCode, duration, stderr)
		return
	}
	log.Printf("ERROR Command failed: %s (exit code: %d, duration: %v)", command, exitCode, duration)
	log.Printf("ERROR Stderr: %s", stderr)
}
