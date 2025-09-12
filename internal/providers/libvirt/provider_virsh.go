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

package libvirt

import (
	"context"
	"fmt"
	"log"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Clean provider implementation using only virsh

// Create creates a new VM using virsh
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	log.Printf("INFO Creating VM: %s", req.Name)

	if p.virshProvider == nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if domain already exists
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return contracts.CreateResponse{}, contracts.NewRetryableError("failed to list existing domains", err)
	}

	for _, domain := range domains {
		if domain.Name == req.Name {
			log.Printf("INFO Domain %s already exists with state: %s", req.Name, domain.State)
			return contracts.CreateResponse{
				ID: req.Name,
			}, nil
		}
	}

	// TODO: Generate domain XML and create via virsh
	// For now, return success to test the basic flow
	log.Printf("INFO Would create new domain: %s", req.Name)

	return contracts.CreateResponse{
		ID: req.Name,
	}, nil
}

// Delete removes a VM using virsh
func (p *Provider) Delete(ctx context.Context, id string) (taskRef string, err error) {
	log.Printf("INFO Deleting VM: %s", id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Check if domain exists
	domains, err := p.virshProvider.listDomains(ctx)
	if err != nil {
		return "", contracts.NewRetryableError("failed to list domains", err)
	}

	domainExists := false
	for _, domain := range domains {
		if domain.Name == id {
			domainExists = true
			break
		}
	}

	if !domainExists {
		log.Printf("INFO Domain %s does not exist, already deleted", id)
		return "", nil
	}

	// Stop the domain if running
	if err := p.virshProvider.destroyDomain(ctx, id); err != nil {
		log.Printf("WARN Failed to destroy domain %s: %v", id, err)
		// Continue with undefine even if destroy fails
	}

	// Remove the domain definition
	if err := p.virshProvider.undefineDomain(ctx, id); err != nil {
		return "", contracts.NewRetryableError("failed to undefine domain", err)
	}

	log.Printf("INFO Successfully deleted domain: %s", id)
	return "", nil
}

// Power controls VM power state using virsh
func (p *Provider) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, err error) {
	log.Printf("INFO Power operation %s on VM: %s", op, id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	switch op {
	case contracts.PowerOpOn:
		err = p.virshProvider.startDomain(ctx, id)
	case contracts.PowerOpOff:
		err = p.virshProvider.stopDomain(ctx, id)
	case contracts.PowerOpReboot:
		// Restart by stopping then starting
		if stopErr := p.virshProvider.stopDomain(ctx, id); stopErr != nil {
			log.Printf("WARN Failed to stop domain for reboot: %v", stopErr)
		}
		err = p.virshProvider.startDomain(ctx, id)
	default:
		return "", contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
	}

	if err != nil {
		return "", contracts.NewRetryableError(fmt.Sprintf("failed to perform power operation %s", op), err)
	}

	log.Printf("INFO Successfully performed power operation %s on %s", op, id)
	return "", nil
}

// Reconfigure updates VM configuration using virsh
func (p *Provider) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, err error) {
	log.Printf("INFO Reconfiguring VM: %s", id)

	if p.virshProvider == nil {
		return "", contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// TODO: Implement reconfiguration via virsh
	log.Printf("INFO Would reconfigure domain: %s", id)

	return "", nil
}

// Describe returns VM information using virsh
func (p *Provider) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	log.Printf("INFO Describing VM: %s", id)

	if p.virshProvider == nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Get domain information
	domainInfo, err := p.virshProvider.getDomainInfo(ctx, id)
	if err != nil {
		return contracts.DescribeResponse{}, contracts.NewRetryableError("failed to get domain info", err)
	}

	// Convert virsh domain info to contracts format
	response := contracts.DescribeResponse{
		Exists:     true,
		PowerState: domainInfo["State"],
		// TODO: Add more fields from domainInfo
	}

	log.Printf("INFO Domain %s state: %s", id, response.PowerState)
	return response, nil
}

// IsTaskComplete checks if a task is complete (virsh operations are usually synchronous)
func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	// Most virsh operations are synchronous, so tasks are immediately complete
	return true, nil
}
