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
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestHandleReadyPhase_DeleteAfterMigrationWithTargetOffWarns: since
// spec.target.powerOn is honored (default false), a migration with
// spec.source.deleteAfterMigration: true and powerOn false deletes its source
// while its target was never started — the workload then runs nowhere. That
// is allowed, but it is announced with a Warning event; with powerOn: true it
// is not.
func TestHandleReadyPhase_DeleteAfterMigrationWithTargetOffWarns(t *testing.T) {
	for name, tc := range map[string]struct {
		powerOn  bool
		wantWarn bool
	}{
		"powerOn: false": {powerOn: false, wantWarn: true},
		"powerOn: true":  {powerOn: true, wantWarn: false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sourceVM, sourceProvider, migration := snapshotFixture()
			migration.Status.SnapshotID = ""
			migration.Spec.Source.DeleteAfterMigration = true
			migration.Spec.Target.PowerOn = tc.powerOn
			migration.Status.Phase = infrav1beta1.MigrationPhaseReady
			r, c := newSnapshotReconciler(t, &countingSnapshotProvider{}, sourceVM, sourceProvider, migration)

			_, err := r.handleReadyPhase(ctx, migration)
			require.NoError(t, err)

			err = c.Get(ctx, client.ObjectKeyFromObject(sourceVM), &infrav1beta1.VirtualMachine{})
			assert.True(t, apierrors.IsNotFound(err), "the source is deleted either way")

			var events []string
			rec, ok := r.Recorder.(*record.FakeRecorder)
			require.True(t, ok)
			for len(rec.Events) > 0 {
				events = append(events, <-rec.Events)
			}
			joined := strings.Join(events, "\n")
			if !tc.wantWarn {
				assert.NotContains(t, joined, migrationReasonSourceDeletedTargetNotStarted)
				return
			}
			assert.Contains(t, joined, "Warning "+migrationReasonSourceDeletedTargetNotStarted)
			assert.Contains(t, joined, "default/source-vm was deleted")
			assert.Contains(t, joined, "default/target-vm was never started")
		})
	}
}
