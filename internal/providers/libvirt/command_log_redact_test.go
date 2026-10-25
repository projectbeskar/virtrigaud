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
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// r28StagedURL is a staged disk's nfs:// URL as the host-side qemu-img gets
// it: the tenant's server, export, path and AUTH_SYS identity.
const r28StagedURL = "nfs://nfs.example.com/exports/migrations/ns1/mig-a/disk.qcow2?uid=1000&gid=1000"

// TestRedactCommandText: every URL but the libvirt connection URI and a pvc://
// location is replaced by a marker naming its scheme; the command name and the other
// arguments stay.
func TestRedactCommandText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no URL": {"virsh -c qemu:///system domstate web", "virsh -c qemu:///system domstate web"},
		"remote quoted nfs convert": {
			"'sudo' '-n' 'qemu-img' 'convert' '-U' '-f' 'qcow2' '-O' 'qcow2' '/var/lib/libvirt/images/web-disk.qcow2' '" + r28StagedURL + "'",
			"'sudo' '-n' 'qemu-img' 'convert' '-U' '-f' 'qcow2' '-O' 'qcow2' '/var/lib/libvirt/images/web-disk.qcow2' '<redacted nfs URL>'",
		},
		"local nfs import": {
			"qemu-img convert -f qcow2 -O qcow2 " + r28StagedURL + " /var/lib/libvirt/images/.w/disk.qcow2",
			"qemu-img convert -f qcow2 -O qcow2 <redacted nfs URL> /var/lib/libvirt/images/.w/disk.qcow2",
		},
		"qemu-img stderr": {
			"qemu-img: Could not open '" + r28StagedURL + "': Permission denied",
			"qemu-img: Could not open '<redacted nfs URL>': Permission denied",
		},
		"s3 and https": {
			"dest=s3://tenant-bucket/ns1/mig-a/disk.qcow2 endpoint=https://user:secret@s3.example.com/x?sig=abc",
			"dest=<redacted s3 URL> endpoint=<redacted https URL>",
		},
		"libvirt URIs stay": {
			"virsh -c qemu+ssh://virt@host.example.com/system list; virsh -c qemu:///system list",
			"virsh -c qemu+ssh://virt@host.example.com/system list; virsh -c qemu:///system list",
		},
		"pvc stays": {"import from pvc://mig-pvc/ns1/disk.qcow2", "import from pvc://mig-pvc/ns1/disk.qcow2"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, redactCommandText(tc.in))
		})
	}
}

// TestLogCommandFailure_RedactsLocations: the shared transport's failure log
// (ERROR and the probes' DEBUG line) never prints a staged disk's URL, in the
// command line or in its stderr, and keeps the command and its exit status.
func TestLogCommandFailure_RedactsLocations(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cmd := "'qemu-img' 'convert' '-f' 'qcow2' '-O' 'qcow2' '/var/lib/libvirt/images/web-disk.qcow2' '" + r28StagedURL + "'"
	stderr := "qemu-img: Could not open '" + r28StagedURL + "': Permission denied"
	logCommandFailure(context.Background(), cmd, 1, time.Millisecond, stderr)
	logCommandFailure(withExpectedFailures(context.Background()), cmd, 1, time.Millisecond, stderr)

	out := buf.String()
	assert.Contains(t, out, "ERROR Command failed: 'qemu-img' 'convert'")
	assert.Contains(t, out, "<redacted nfs URL>")
	assert.Contains(t, out, "exit code: 1")
	assert.Contains(t, out, "DEBUG Probe command failed")
	for _, leak := range []string{"nfs.example.com", "/exports/migrations", "uid=1000", "mig-a"} {
		assert.NotContains(t, out, leak)
	}
}

// TestVirshErrorText_RedactsLocations: a failed command's error text — which
// callers log — carries no staged URL; its fields keep the original text for
// classification.
func TestVirshErrorText_RedactsLocations(t *testing.T) {
	ve := &VirshError{
		Command:  "qemu-img convert " + r28StagedURL + " /tmp/x",
		ExitCode: 1,
		Stderr:   "qemu-img: Could not open '" + r28StagedURL + "'",
		Stdout:   "wrote " + r28StagedURL,
	}
	assert.NotContains(t, ve.Error(), "nfs.example.com")
	assert.Contains(t, ve.Error(), "qemu-img convert <redacted nfs URL> /tmp/x")
	assert.Contains(t, ve.Stderr, "nfs.example.com", "the field keeps the text the classifiers read")
}

// TestStagedDiskLocations: the s3 bucket is redacted with the key; a value
// too short to be a bucket name is not used.
func TestStagedDiskLocations(t *testing.T) {
	disk := migration.StagedDisk{Backend: migration.BackendS3, Key: "ns1/mig-a/disk.qcow2",
		Options: migration.StorageOptions{Bucket: "tenant-bucket"}}
	err := redactLocations(errors.New("PutObject tenant-bucket/ns1/mig-a/disk.qcow2: AccessDenied"), stagedDiskLocations(disk)...)
	assert.Equal(t, "PutObject "+stagedLocationMarker+"/"+stagedLocationMarker+": AccessDenied", err.Error())

	short := migration.StagedDisk{Key: "k1", Options: migration.StorageOptions{Bucket: "ab"}}
	assert.Equal(t, []string{"", "k1"}, stagedDiskLocations(short))
}
