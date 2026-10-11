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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestMigrationSnapshotIdentity_FitsTheSmallestNameLimit (re-review L-1): a
// long VM name is cut, deterministically, so the name hint with its attempt
// suffix fits Proxmox's 40-character snapshot names.
func TestMigrationSnapshotIdentity_FitsTheSmallestNameLimit(t *testing.T) {
	m := &infrav1beta1.VMMigration{}
	m.UID = "0123456789abcdef"
	m.Spec.Source.VMRef.Name = "a-very-long-virtual-machine-name-in-a-long-namespace"
	for _, retries := range []int32{0, 1, 10, 99} {
		m.Status.RetryCount = retries
		name, _ := migrationSnapshotIdentity(m)
		assert.LessOrEqual(t, len(name), migrationSnapshotNameMax, "retry %d: %q", retries, name)
		again, _ := migrationSnapshotIdentity(m)
		assert.Equal(t, name, again, "deterministic")
	}
	m.Status.RetryCount = 0
	name, _ := migrationSnapshotIdentity(m)
	assert.Equal(t, "a-very-long-virtu-migration-01234567", name)

	m.Spec.Source.VMRef.Name = "web"
	name, _ = migrationSnapshotIdentity(m)
	assert.Equal(t, "web-migration-01234567", name, "a short name is unchanged")
}
