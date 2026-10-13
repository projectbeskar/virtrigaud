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
	"bytes"
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestLogCommandFailure_ProbesAreDebug: a failed best-effort probe (Describe's
// statistics and guest-agent reads) is logged at DEBUG; any other failed
// command keeps its ERROR lines.
func TestLogCommandFailure_ProbesAreDebug(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	logCommandFailure(withExpectedFailures(context.Background()), "virsh dommemstat web", 1, time.Millisecond,
		"error: Requested operation is not valid: domain is not running")
	assert.Contains(t, buf.String(), "DEBUG Probe command failed")
	assert.NotContains(t, buf.String(), "ERROR")

	buf.Reset()
	logCommandFailure(context.Background(), "virsh destroy web", 1, time.Millisecond, "error: boom")
	assert.Contains(t, buf.String(), "ERROR Command failed: virsh destroy web")
	assert.Contains(t, buf.String(), "ERROR Stderr: error: boom")
}
