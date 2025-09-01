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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// findTemplate finds a VM template by name from the image specification
func (p *Provider) findTemplate(ctx context.Context, image contracts.VMImage) (*object.VirtualMachine, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	// Use template name from image specification
	templateName := image.TemplateName
	if templateName == "" {
		return nil, fmt.Errorf("no template name specified in image")
	}

	// Find the template
	templates, err := p.finder.VirtualMachineList(ctx, templateName)
	if err != nil {
		return nil, fmt.Errorf("failed to find template %s: %w", templateName, err)
	}

	if len(templates) == 0 {
		return nil, fmt.Errorf("template not found: %s", templateName)
	}

	return templates[0], nil
}

// findResourcePool finds a resource pool based on placement hints
func (p *Provider) findResourcePool(ctx context.Context, placement *contracts.Placement) (*object.ResourcePool, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	var clusterName string

	// Use placement cluster if specified
	if placement != nil && placement.Cluster != "" {
		clusterName = placement.Cluster
	} else if p.config.Spec.Defaults != nil && p.config.Spec.Defaults.Cluster != "" {
		// Fall back to default cluster
		clusterName = p.config.Spec.Defaults.Cluster
	}

	if clusterName != "" {
		// Find cluster and get its resource pool
		cluster, err := p.finder.ClusterComputeResource(ctx, clusterName)
		if err != nil {
			return nil, fmt.Errorf("failed to find cluster %s: %w", clusterName, err)
		}

		resourcePool, err := cluster.ResourcePool(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get resource pool for cluster %s: %w", clusterName, err)
		}

		return resourcePool, nil
	}

	// No specific cluster, find default resource pool
	resourcePools, err := p.finder.ResourcePoolList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("failed to find resource pools: %w", err)
	}

	if len(resourcePools) == 0 {
		return nil, fmt.Errorf("no resource pools found")
	}

	// Return the first available resource pool
	return resourcePools[0], nil
}

// findDatastore finds a datastore based on placement hints
func (p *Provider) findDatastore(ctx context.Context, placement *contracts.Placement) (*object.Datastore, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	var datastoreName string

	// Use placement datastore if specified
	if placement != nil && placement.Datastore != "" {
		datastoreName = placement.Datastore
	} else if p.config.Spec.Defaults != nil && p.config.Spec.Defaults.Datastore != "" {
		// Fall back to default datastore
		datastoreName = p.config.Spec.Defaults.Datastore
	}

	if datastoreName != "" {
		datastore, err := p.finder.Datastore(ctx, datastoreName)
		if err != nil {
			return nil, fmt.Errorf("failed to find datastore %s: %w", datastoreName, err)
		}
		return datastore, nil
	}

	// No specific datastore, find the one with most free space
	datastores, err := p.finder.DatastoreList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("failed to find datastores: %w", err)
	}

	if len(datastores) == 0 {
		return nil, fmt.Errorf("no datastores found")
	}

	// For MVP, return the first datastore
	// TODO: Implement logic to select datastore with most free space
	return datastores[0], nil
}

// findFolder finds a folder based on placement hints
func (p *Provider) findFolder(ctx context.Context, placement *contracts.Placement) (*object.Folder, error) {
	if err := p.ensureConnection(ctx); err != nil {
		return nil, err
	}

	var folderPath string

	// Use placement folder if specified
	if placement != nil && placement.Folder != "" {
		folderPath = placement.Folder
	} else if p.config.Spec.Defaults != nil && p.config.Spec.Defaults.Folder != "" {
		// Fall back to default folder
		folderPath = p.config.Spec.Defaults.Folder
	}

	if folderPath != "" {
		folder, err := p.finder.Folder(ctx, folderPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find folder %s: %w", folderPath, err)
		}
		return folder, nil
	}

	// No specific folder, use the VM folder of the datacenter
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}

	folders, err := datacenter.Folders(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get datacenter folders: %w", err)
	}

	return folders.VmFolder, nil
}

// findNetwork finds a network/portgroup for VM configuration
