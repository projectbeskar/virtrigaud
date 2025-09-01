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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

var (
	kubeconfig string
	namespace  string
	output     string
	timeout    time.Duration
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "vrtg",
		Short: "CLI tool for virtrigaud",
		Long:  "Command-line interface for managing virtrigaud resources",
	}

	rootCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().StringVarP(&output, "output", "o", "table", "Output format (table|json|yaml)")
	rootCmd.PersistentFlags().DurationVar(&timeout, "timeout", 30*time.Second, "Request timeout")

	// VM commands
	vmCmd := &cobra.Command{
		Use:     "vm",
		Aliases: []string{"virtualmachine", "vms"},
		Short:   "Manage virtual machines",
	}

	vmCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List virtual machines",
			RunE:  listVMs,
		},
		&cobra.Command{
			Use:   "describe <name>",
			Short: "Describe a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  describeVM,
		},
		&cobra.Command{
			Use:   "events <name>",
			Short: "Show events for a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  vmEvents,
		},
		&cobra.Command{
			Use:   "console-url <name>",
			Short: "Get console URL for a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  vmConsoleURL,
		},
	)

	// Provider commands
	providerCmd := &cobra.Command{
		Use:     "provider",
		Aliases: []string{"providers"},
		Short:   "Manage providers",
	}

	providerCmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List providers",
			RunE:  listProviders,
		},
		&cobra.Command{
			Use:   "status <name>",
			Short: "Show provider status",
			Args:  cobra.ExactArgs(1),
			RunE:  providerStatus,
		},
		&cobra.Command{
			Use:   "logs <name>",
			Short: "Show provider logs",
			Args:  cobra.ExactArgs(1),
			RunE:  providerLogs,
		},
	)

	// Snapshot commands
	snapshotCmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snap", "snapshots"},
		Short:   "Manage VM snapshots",
	}

	snapshotCmd.AddCommand(
		&cobra.Command{
			Use:   "create <vm-name> <snapshot-name>",
			Short: "Create a VM snapshot",
			Args:  cobra.ExactArgs(2),
			RunE:  createSnapshot,
		},
		&cobra.Command{
			Use:   "list [vm-name]",
			Short: "List snapshots",
			Args:  cobra.MaximumNArgs(1),
			RunE:  listSnapshots,
		},
		&cobra.Command{
			Use:   "revert <vm-name> <snapshot-name>",
			Short: "Revert VM to snapshot",
			Args:  cobra.ExactArgs(2),
			RunE:  revertSnapshot,
		},
	)

	// Clone commands
	cloneCmd := &cobra.Command{
		Use:     "clone",
		Aliases: []string{"clones"},
		Short:   "Manage VM clones",
	}

	cloneCmd.AddCommand(
		&cobra.Command{
			Use:   "run <source-vm> <target-vm>",
			Short: "Clone a virtual machine",
			Args:  cobra.ExactArgs(2),
			RunE:  runClone,
		},
		&cobra.Command{
			Use:   "list",
			Short: "List clone operations",
			RunE:  listClones,
		},
	)

	// Conformance commands
	conformanceCmd := &cobra.Command{
		Use:     "conformance",
		Aliases: []string{"conf"},
		Short:   "Run conformance tests",
	}

	conformanceCmd.AddCommand(
		&cobra.Command{
			Use:   "run <provider>",
			Short: "Run conformance tests against a provider",
			Args:  cobra.ExactArgs(1),
			RunE:  runConformance,
		},
	)

	// Diagnostics commands
	diagCmd := &cobra.Command{
		Use:     "diag",
		Aliases: []string{"diagnostics"},
		Short:   "Diagnostic tools",
	}

	diagCmd.AddCommand(
		&cobra.Command{
			Use:   "bundle",
			Short: "Create diagnostic bundle",
			RunE:  createDiagBundle,
		},
	)

	// Installation commands
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize virtrigaud",
		Long:  "Install virtrigaud CRDs and components",
		RunE:  initVirtrigaud,
	}

	rootCmd.AddCommand(vmCmd, providerCmd, snapshotCmd, cloneCmd, conformanceCmd, diagCmd, initCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func listVMs(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	vmList := &infrav1beta1.VirtualMachineList{}
	err = client.List(ctx, vmList)
	if err != nil {
		return fmt.Errorf("failed to list VMs: %w", err)
	}

	if output == "table" {
		fmt.Printf("%-20s %-15s %-15s %-15s %-20s %-10s\n",
			"NAME", "PROVIDER", "CLASS", "IMAGE", "IPS", "AGE")
		for _, vm := range vmList.Items {
			age := time.Since(vm.CreationTimestamp.Time).Truncate(time.Second)
			ips := strings.Join(vm.Status.IPs, ",")
			if ips == "" {
				ips = "<none>"
			}
			fmt.Printf("%-20s %-15s %-15s %-15s %-20s %-10s\n",
				vm.Name, vm.Spec.ProviderRef.Name, vm.Spec.ClassRef.Name,
				vm.Spec.ImageRef.Name, ips, age)
		}
	} else {
		return outputResource(vmList)
	}

	return nil
}

func describeVM(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	vm := &infrav1beta1.VirtualMachine{}
	key := types.NamespacedName{Namespace: namespace, Name: args[0]}
	err = client.Get(ctx, key, vm)
	if err != nil {
		return fmt.Errorf("failed to get VM: %w", err)
	}

	if output == "table" {
		fmt.Printf("Name: %s\n", vm.Name)
		fmt.Printf("Namespace: %s\n", vm.Namespace)
		fmt.Printf("Provider: %s\n", vm.Spec.ProviderRef.Name)
		fmt.Printf("Class: %s\n", vm.Spec.ClassRef.Name)
		fmt.Printf("Image: %s\n", vm.Spec.ImageRef.Name)
		fmt.Printf("Power State: %s\n", vm.Spec.PowerState)
		fmt.Printf("VM ID: %s\n", vm.Status.ID)
		fmt.Printf("Current Power State: %s\n", vm.Status.PowerState)
		fmt.Printf("IPs: %s\n", strings.Join(vm.Status.IPs, ", "))
		fmt.Printf("Console URL: %s\n", vm.Status.ConsoleURL)
		fmt.Printf("Created: %s\n", vm.CreationTimestamp.Time.Format(time.RFC3339))

		if len(vm.Status.Conditions) > 0 {
			fmt.Printf("\nConditions:\n")
			for _, condition := range vm.Status.Conditions {
				fmt.Printf("  %s: %s (%s)\n", condition.Type, condition.Status, condition.Reason)
			}
		}
	} else {
		return outputResource(vm)
	}

	return nil
}

func vmEvents(cmd *cobra.Command, args []string) error {
	clientset, err := getClientset()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=VirtualMachine", args[0]),
	})
	if err != nil {
		return fmt.Errorf("failed to get events: %w", err)
	}

	fmt.Printf("%-30s %-10s %-15s %-50s\n", "LAST SEEN", "TYPE", "REASON", "MESSAGE")
	for _, event := range events.Items {
		lastSeen := event.LastTimestamp.Time.Format("2006-01-02 15:04:05")
		fmt.Printf("%-30s %-10s %-15s %-50s\n",
			lastSeen, event.Type, event.Reason, event.Message)
	}

	return nil
}

func vmConsoleURL(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	vm := &infrav1beta1.VirtualMachine{}
	key := types.NamespacedName{Namespace: namespace, Name: args[0]}
	err = client.Get(ctx, key, vm)
	if err != nil {
		return fmt.Errorf("failed to get VM: %w", err)
	}

	if vm.Status.ConsoleURL == "" {
		fmt.Printf("No console URL available for VM %s\n", args[0])
	} else {
		fmt.Printf("%s\n", vm.Status.ConsoleURL)
	}

	return nil
}

func listProviders(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	providerList := &infrav1beta1.ProviderList{}
	err = client.List(ctx, providerList)
	if err != nil {
		return fmt.Errorf("failed to list providers: %w", err)
	}

	if output == "table" {
		fmt.Printf("%-20s %-15s %-15s %-10s\n", "NAME", "TYPE", "ENDPOINT", "AGE")
		for _, provider := range providerList.Items {
			age := time.Since(provider.CreationTimestamp.Time).Truncate(time.Second)
			fmt.Printf("%-20s %-15s %-15s %-10s\n",
				provider.Name, provider.Spec.Type, provider.Spec.Endpoint, age)
		}
	} else {
		return outputResource(providerList)
	}

	return nil
}

func providerStatus(cmd *cobra.Command, args []string) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	provider := &infrav1beta1.Provider{}
	key := types.NamespacedName{Namespace: namespace, Name: args[0]}
	err = client.Get(ctx, key, provider)
	if err != nil {
		return fmt.Errorf("failed to get provider: %w", err)
	}

	fmt.Printf("Provider: %s\n", provider.Name)
	fmt.Printf("Type: %s\n", provider.Spec.Type)
	fmt.Printf("Endpoint: %s\n", provider.Spec.Endpoint)
	fmt.Printf("Status: Available\n")

	if len(provider.Status.Conditions) > 0 {
		fmt.Printf("\nConditions:\n")
		for _, condition := range provider.Status.Conditions {
			fmt.Printf("  %s: %s (%s)\n", condition.Type, condition.Status, condition.Reason)
		}
	}

	fmt.Printf("\nNote: Use kubectl describe for full provider details\n")

	return nil
}

func providerLogs(cmd *cobra.Command, args []string) error {
	fmt.Printf("Provider logs for %s (not implemented - use kubectl logs)\n", args[0])
	return nil
}

func createSnapshot(cmd *cobra.Command, args []string) error {
	fmt.Printf("Creating snapshot %s for VM %s (not implemented)\n", args[1], args[0])
	return nil
}

func listSnapshots(cmd *cobra.Command, args []string) error {
	fmt.Printf("Listing snapshots (not implemented)\n")
	return nil
}

func revertSnapshot(cmd *cobra.Command, args []string) error {
	fmt.Printf("Reverting VM %s to snapshot %s (not implemented)\n", args[0], args[1])
	return nil
}

func runClone(cmd *cobra.Command, args []string) error {
	fmt.Printf("Cloning VM %s to %s (not implemented)\n", args[0], args[1])
	return nil
}

func listClones(cmd *cobra.Command, args []string) error {
	fmt.Printf("Listing clone operations (not implemented)\n")
	return nil
}

func runConformance(cmd *cobra.Command, args []string) error {
	fmt.Printf("Running conformance tests against provider %s (not implemented)\n", args[0])
	return nil
}

func createDiagBundle(cmd *cobra.Command, args []string) error {
	fmt.Printf("Creating diagnostic bundle (not implemented)\n")
	return nil
}

func initVirtrigaud(cmd *cobra.Command, args []string) error {
	fmt.Printf("Initializing virtrigaud (not implemented)\n")
	return nil
}

func getClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{})
}

func getClientset() (kubernetes.Interface, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func outputResource(obj interface{}) error {
	// Implementation for JSON/YAML output would go here
	fmt.Printf("JSON/YAML output not implemented\n")
	return nil
}
