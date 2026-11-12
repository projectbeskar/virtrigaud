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

package proxmox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestTaskStatus_NoTaskReferenceIsInvalidArgument: a TaskStatus without a
// task reference, or with an empty one, is refused with
// codes.InvalidArgument instead of dereferencing a missing task.
func TestTaskStatus_NoTaskReferenceIsInvalidArgument(t *testing.T) {
	provider, _ := ciProvider(t, "")
	for name, req := range map[string]*providerv1.TaskStatusRequest{
		"no task":  {},
		"empty id": {Task: &providerv1.TaskRef{}},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := provider.TaskStatus(context.Background(), req)
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Equal(t, taskReferenceRequiredMessage, status.Convert(err).Message())
		})
	}
}

// TestTaskStatus_NoTaskReferenceBeforeClient: the reference is checked
// before the PVE client, so an unconfigured provider refuses a task-less
// request the same way.
func TestTaskStatus_NoTaskReferenceBeforeClient(t *testing.T) {
	_, err := (&Provider{}).TaskStatus(context.Background(), &providerv1.TaskStatusRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
