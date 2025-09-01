/*
Copyright 2025.

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

package vsphere

import (
	"context"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// getTask retrieves a task by its reference
func (p *Provider) getTask(ctx context.Context, taskRef string) (*object.Task, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	ref := types.ManagedObjectReference{
		Type:  "Task",
		Value: taskRef,
	}

	return object.NewTask(p.client.Client, ref), nil
}

// waitForTask waits for a task to complete and returns the result

// getTaskResult gets the result of a completed task

// isTaskComplete checks if a task is complete

// getVMFromTask extracts VM reference from a completed task
