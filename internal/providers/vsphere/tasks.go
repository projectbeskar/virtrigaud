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
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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
func (p *Provider) waitForTask(ctx context.Context, task *object.Task) (*types.TaskInfo, error) {
	return task.WaitForResult(ctx, nil)
}

// getTaskResult gets the result of a completed task
func (p *Provider) getTaskResult(ctx context.Context, taskRef string) (*types.TaskInfo, error) {
	task, err := p.getTask(ctx, taskRef)
	if err != nil {
		return nil, err
	}

	// Get task info without waiting
	var taskInfo types.TaskInfo
	err = task.Properties(ctx, task.Reference(), []string{"info"}, &taskInfo)
	if err != nil {
		return nil, contracts.NewRetryableError("failed to get task info", err)
	}

	return &taskInfo, nil
}

// isTaskComplete checks if a task is complete
func (p *Provider) isTaskComplete(ctx context.Context, taskRef string) (bool, error) {
	taskInfo, err := p.getTaskResult(ctx, taskRef)
	if err != nil {
		return false, err
	}

	switch taskInfo.State {
	case types.TaskInfoStateSuccess:
		return true, nil
	case types.TaskInfoStateError:
		// Task completed with error
		if taskInfo.Error != nil {
			return true, contracts.NewRetryableError("task failed", fmt.Errorf(taskInfo.Error.LocalizedMessage))
		}
		return true, contracts.NewRetryableError("task failed with unknown error", nil)
	case types.TaskInfoStateRunning:
		return false, nil
	case types.TaskInfoStateQueued:
		return false, nil
	default:
		return false, contracts.NewRetryableError(fmt.Sprintf("unknown task state: %s", taskInfo.State), nil)
	}
}

// getVMFromTask extracts VM reference from a completed task
func (p *Provider) getVMFromTask(ctx context.Context, taskRef string) (*object.VirtualMachine, error) {
	taskInfo, err := p.getTaskResult(ctx, taskRef)
	if err != nil {
		return nil, err
	}

	if taskInfo.State != types.TaskInfoStateSuccess {
		return nil, fmt.Errorf("task not completed successfully: %s", taskInfo.State)
	}

	if taskInfo.Result == nil {
		return nil, fmt.Errorf("task result is nil")
	}

	// The result should be a VirtualMachine reference
	vmRef, ok := taskInfo.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("task result is not a VM reference")
	}

	return object.NewVirtualMachine(p.client.Client, vmRef), nil
}
