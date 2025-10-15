package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	// Connect to Proxmox provider service
	// Use the service DNS name from within the cluster
	// Or port-forward: kubectl port-forward -n default svc/virtrigaud-provider-default-proxmox-prod 9090:9090
	target := "localhost:9090" // After port-forward

	log.Printf("Connecting to Proxmox provider at %s...", target)

	conn, err := grpc.Dial(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	client := providerv1.NewProviderServiceClient(conn)
	ctx := context.Background()

	// Test 1: Validate connectivity
	log.Println("\n=== Test 1: Validate Provider ===")
	validateResp, err := client.Validate(ctx, &providerv1.ValidateRequest{})
	if err != nil {
		log.Fatalf("Validate failed: %v", err)
	}
	log.Printf("Validate response: ok=%v, message=%s", validateResp.Ok, validateResp.Message)

	if !validateResp.Ok {
		log.Fatalf("Provider validation failed!")
	}

	// Test 2: Create VM
	log.Println("\n=== Test 2: Create VM ===")

	// Prepare VMClass JSON
	vmClass := map[string]interface{}{
		"cpu":    2,
		"memory": "4Gi",
		"diskDefaults": map[string]interface{}{
			"size": "20Gi",
			"type": "scsi",
		},
	}
	classJSON, _ := json.Marshal(vmClass)

	// Prepare VMImage JSON with Proxmox template
	vmImage := map[string]interface{}{
		"source": map[string]interface{}{
			// The provider code should accept this in parseCreateRequest
			"template": "9000", // Template VMID in Proxmox
			"storage":  "vms",  // Storage pool
		},
	}
	imageJSON, _ := json.Marshal(vmImage)

	// Prepare Network JSON
	networks := []map[string]interface{}{
		{
			"name": "eth0",
			"config": map[string]interface{}{
				"bridge": "vmbr0",
				"model":  "virtio",
			},
		},
	}
	networksJSON, _ := json.Marshal(networks)

	// Cloud-init user data
	userData := []byte(`#cloud-config
hostname: grpc-test-vm
users:
  - name: wrkode
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7lHIuo2QJBkdVDL79bl+tEmJh3pBz7rHImwvNMjenK
packages:
  - curl
  - htop
`)

	// Placement JSON (optional - for node selection)
	placement := map[string]interface{}{
		"node": "pve", // Adjust to your Proxmox node name
	}
	placementJSON, _ := json.Marshal(placement)

	createReq := &providerv1.CreateRequest{
		Name:          "grpc-test-vm-001",
		UserData:      userData,
		ClassJson:     string(classJSON),
		ImageJson:     string(imageJSON),
		NetworksJson:  string(networksJSON),
		PlacementJson: string(placementJSON),
		Tags:          []string{"test", "grpc", "virtrigaud"},
	}

	log.Printf("Sending Create request with:")
	log.Printf("  Name: %s", createReq.Name)
	log.Printf("  Class: %s", createReq.ClassJson)
	log.Printf("  Image: %s", createReq.ImageJson)
	log.Printf("  Networks: %s", createReq.NetworksJson)
	log.Printf("  Placement: %s", createReq.PlacementJson)

	createResp, err := client.Create(ctx, createReq)
	if err != nil {
		log.Fatalf("Create failed: %v", err)
	}

	log.Printf("✅ VM Created!")
	log.Printf("  ID: %s", createResp.Id)
	if createResp.Task != nil {
		log.Printf("  Task ID: %s", createResp.Task.Id)

		// Wait for task to complete
		log.Println("\n=== Waiting for task to complete ===")
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)

			taskResp, err := client.TaskStatus(ctx, &providerv1.TaskStatusRequest{
				Task: createResp.Task,
			})
			if err != nil {
				log.Printf("TaskStatus error: %v", err)
				continue
			}

			log.Printf("Task status: done=%v, error=%s", taskResp.Done, taskResp.Error)

			if taskResp.Done {
				if taskResp.Error != "" {
					log.Fatalf("Task failed: %s", taskResp.Error)
				}
				log.Println("✅ Task completed successfully!")
				break
			}
		}
	}

	// Test 3: Describe VM
	log.Println("\n=== Test 3: Describe VM ===")
	time.Sleep(5 * time.Second)

	descResp, err := client.Describe(ctx, &providerv1.DescribeRequest{
		Id: createResp.Id,
	})
	if err != nil {
		log.Fatalf("Describe failed: %v", err)
	}

	log.Printf("VM Details:")
	log.Printf("  Exists: %v", descResp.Exists)
	log.Printf("  Power State: %s", descResp.PowerState)
	log.Printf("  IPs: %v", descResp.Ips)
	log.Printf("  Console URL: %s", descResp.ConsoleUrl)
	log.Printf("  Provider Data: %s", descResp.ProviderRawJson)

	// Test 4: Power operations
	log.Println("\n=== Test 4: Power On ===")
	powerReq := &providerv1.PowerRequest{
		Id: createResp.Id,
		Op: providerv1.PowerOp_POWER_OP_ON,
	}

	_, err = client.Power(ctx, powerReq)
	if err != nil {
		log.Printf("Power On warning: %v", err)
	} else {
		log.Println("✅ Power On command sent")
	}

	// Give it time to power on
	time.Sleep(5 * time.Second)

	// Describe again to see updated state
	descResp2, err := client.Describe(ctx, &providerv1.DescribeRequest{
		Id: createResp.Id,
	})
	if err == nil {
		log.Printf("VM Power State after power on: %s", descResp2.PowerState)
	}

	log.Println("\n=== ✅ All tests completed! ===")
	log.Printf("VM ID: %s", createResp.Id)
	log.Println("Check your Proxmox UI to verify the VM was created!")
	log.Println("\nTo clean up, run:")
	log.Printf("  # Delete via gRPC or manually in Proxmox")
}
