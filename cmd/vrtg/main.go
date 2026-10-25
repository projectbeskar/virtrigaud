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

// Package main provides vrtg, the command-line tool for inspecting VirtRigaud
// resources in a cluster.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/yaml"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
)

const (
	// outputTable, outputJSON and outputYAML are the values of --output.
	outputTable = "table"
	outputJSON  = "json"
	outputYAML  = "yaml"

	// noneValue is printed in a table cell with nothing to show.
	noneValue = "<none>"

	// flagNamespace is the name of the --namespace flag.
	flagNamespace = "namespace"
)

// errNotImplemented is returned by the subcommands that exist in the command
// tree but have no implementation yet.
var errNotImplemented = errors.New("not implemented in this release")

// options holds the values of vrtg's persistent flags.
type options struct {
	kubeconfig string
	namespace  string
	output     string
	timeout    time.Duration
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the vrtg command tree. Every call returns an independent
// tree with its own flag values, so it can be run repeatedly in one process.
func newRootCmd() *cobra.Command {
	opts := &options{}

	rootCmd := &cobra.Command{
		Use:           "vrtg",
		Short:         "CLI tool for virtrigaud",
		Long:          "Command-line interface for managing virtrigaud resources",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&opts.namespace, flagNamespace, "n", "default",
		"Kubernetes namespace (list commands show every namespace unless this is set)")
	rootCmd.PersistentFlags().StringVarP(&opts.output, "output", "o", outputTable, "Output format (table|json|yaml)")
	rootCmd.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second, "Request timeout")

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
			Args:  cobra.NoArgs,
			RunE:  opts.listVMs,
		},
		&cobra.Command{
			Use:   "describe <name>",
			Short: "Describe a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  opts.describeVM,
		},
		&cobra.Command{
			Use:   "events <name>",
			Short: "Show events for a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  opts.vmEvents,
		},
		&cobra.Command{
			Use:   "console-url <name>",
			Short: "Get console URL for a virtual machine",
			Args:  cobra.ExactArgs(1),
			RunE:  opts.vmConsoleURL,
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
			Args:  cobra.NoArgs,
			RunE:  opts.listProviders,
		},
		&cobra.Command{
			Use:   "status <name>",
			Short: "Show provider status",
			Args:  cobra.ExactArgs(1),
			RunE:  opts.providerStatus,
		},
		&cobra.Command{
			Use:   "logs <name>",
			Short: "Show provider logs (not implemented - use kubectl logs)",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("provider logs", "kubectl logs"),
		},
	)

	// Snapshot commands
	snapshotCmd := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snap", "snapshots"},
		Short:   "Manage VM snapshots (not implemented - use VMSnapshot resources)",
	}

	snapshotCmd.AddCommand(
		&cobra.Command{
			Use:   "create <vm-name> <snapshot-name>",
			Short: "Create a VM snapshot (not implemented)",
			Args:  cobra.ExactArgs(2),
			RunE:  notImplemented("snapshot create", "a VMSnapshot resource"),
		},
		&cobra.Command{
			Use:   "list [vm-name]",
			Short: "List snapshots (not implemented)",
			Args:  cobra.MaximumNArgs(1),
			RunE:  notImplemented("snapshot list", "kubectl get vmsnapshots"),
		},
		&cobra.Command{
			Use:   "revert <vm-name> <snapshot-name>",
			Short: "Revert VM to snapshot (not implemented)",
			Args:  cobra.ExactArgs(2),
			RunE:  notImplemented("snapshot revert", "spec.snapshot.revertToRef on the VirtualMachine"),
		},
	)

	// Clone commands
	cloneCmd := &cobra.Command{
		Use:     "clone",
		Aliases: []string{"clones"},
		Short:   "Manage VM clones (not implemented - use VMClone resources)",
	}

	cloneCmd.AddCommand(
		&cobra.Command{
			Use:   "run <source-vm> <target-vm>",
			Short: "Clone a virtual machine (not implemented)",
			Args:  cobra.ExactArgs(2),
			RunE:  notImplemented("clone run", "a VMClone resource"),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List clone operations (not implemented)",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("clone list", "kubectl get vmclones"),
		},
	)

	// Conformance commands
	conformanceCmd := &cobra.Command{
		Use:     "conformance",
		Aliases: []string{"conf"},
		Short:   "Run conformance tests (not implemented - use vcts)",
	}

	conformanceCmd.AddCommand(
		&cobra.Command{
			Use:   "run <provider>",
			Short: "Run conformance tests against a provider (not implemented)",
			Args:  cobra.ExactArgs(1),
			RunE:  notImplemented("conformance run", "vcts run"),
		},
	)

	// Diagnostics commands
	diagCmd := &cobra.Command{
		Use:     "diag",
		Aliases: []string{"diagnostics"},
		Short:   "Diagnostic tools (not implemented)",
	}

	diagCmd.AddCommand(
		&cobra.Command{
			Use:   "bundle",
			Short: "Create diagnostic bundle (not implemented)",
			Args:  cobra.NoArgs,
			RunE:  notImplemented("diag bundle", "kubectl describe and kubectl logs"),
		},
	)

	// Installation commands
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize virtrigaud (not implemented - use the Helm chart)",
		Long:  "Install virtrigaud CRDs and components (not implemented - use the Helm chart)",
		Args:  cobra.NoArgs,
		RunE:  notImplemented("init", "the Helm chart"),
	}

	rootCmd.AddCommand(vmCmd, providerCmd, snapshotCmd, cloneCmd, conformanceCmd, diagCmd, initCmd)

	return rootCmd
}

// notImplemented returns a RunE for a subcommand that has no implementation
// yet. It fails (non-zero exit) rather than printing a notice and exiting 0,
// and points at what to use instead.
func notImplemented(name, instead string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("vrtg %s: %w (use %s instead)", name, errNotImplemented, instead)
	}
}

// newContext returns a context bounded by --timeout.
func (o *options) newContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, o.timeout)
}

// listNamespace returns the namespace a list command is restricted to. It is
// empty (every namespace) unless --namespace was given explicitly.
func (o *options) listNamespace(cmd *cobra.Command) string {
	if cmd.Flags().Changed(flagNamespace) {
		return o.namespace
	}
	return metav1.NamespaceAll
}

// refName returns the name an optional object reference points at, or the
// none placeholder when the reference is unset.
func refName(ref *infrav1beta1.ObjectRef) string {
	if ref == nil || ref.Name == "" {
		return noneValue
	}
	return ref.Name
}

func (o *options) listVMs(cmd *cobra.Command, _ []string) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	ns := o.listNamespace(cmd)
	vmList := &infrav1beta1.VirtualMachineList{}
	if err := c.List(ctx, vmList, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("failed to list VMs: %w", err)
	}

	out := cmd.OutOrStdout()
	if o.output != outputTable {
		return o.outputResource(out, c, vmList)
	}

	fmt.Fprintf(out, "%-20s %-20s %-15s %-15s %-15s %-20s %-10s\n",
		"NAMESPACE", "NAME", "PROVIDER", "CLASS", "IMAGE", "IPS", "AGE")
	for i := range vmList.Items {
		vm := &vmList.Items[i]
		age := time.Since(vm.CreationTimestamp.Time).Truncate(time.Second)
		ips := strings.Join(vm.Status.IPs, ",")
		if ips == "" {
			ips = noneValue
		}
		fmt.Fprintf(out, "%-20s %-20s %-15s %-15s %-15s %-20s %-10s\n",
			vm.Namespace, vm.Name, vm.Spec.ProviderRef.Name, vm.Spec.ClassRef.Name,
			refName(vm.Spec.ImageRef), ips, age)
	}

	return nil
}

func (o *options) describeVM(cmd *cobra.Command, args []string) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	vm := &infrav1beta1.VirtualMachine{}
	key := types.NamespacedName{Namespace: o.namespace, Name: args[0]}
	if err := c.Get(ctx, key, vm); err != nil {
		return fmt.Errorf("failed to get VM %s: %w", key, err)
	}

	out := cmd.OutOrStdout()
	if o.output != outputTable {
		return o.outputResource(out, c, vm)
	}

	fmt.Fprintf(out, "Name: %s\n", vm.Name)
	fmt.Fprintf(out, "Namespace: %s\n", vm.Namespace)
	fmt.Fprintf(out, "Provider: %s\n", vm.Spec.ProviderRef.Name)
	fmt.Fprintf(out, "Class: %s\n", vm.Spec.ClassRef.Name)
	fmt.Fprintf(out, "Image: %s\n", refName(vm.Spec.ImageRef))
	fmt.Fprintf(out, "Power State: %s\n", vm.Spec.PowerState)
	fmt.Fprintf(out, "VM ID: %s\n", vm.Status.ID)
	fmt.Fprintf(out, "Current Power State: %s\n", vm.Status.PowerState)
	fmt.Fprintf(out, "IPs: %s\n", strings.Join(vm.Status.IPs, ", "))
	fmt.Fprintf(out, "Console URL: %s\n", vm.Status.ConsoleURL)
	fmt.Fprintf(out, "Created: %s\n", vm.CreationTimestamp.Format(time.RFC3339))

	if len(vm.Status.Conditions) > 0 {
		fmt.Fprintf(out, "\nConditions:\n")
		for _, condition := range vm.Status.Conditions {
			fmt.Fprintf(out, "  %s: %s (%s)\n", condition.Type, condition.Status, condition.Reason)
		}
	}

	return nil
}

func (o *options) vmEvents(cmd *cobra.Command, args []string) error {
	clientset, err := o.getClientset()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	events, err := clientset.CoreV1().Events(o.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=VirtualMachine", args[0]),
	})
	if err != nil {
		return fmt.Errorf("failed to get events: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-30s %-10s %-15s %-50s\n", "LAST SEEN", "TYPE", "REASON", "MESSAGE")
	for _, event := range events.Items {
		lastSeen := event.LastTimestamp.Format("2006-01-02 15:04:05")
		fmt.Fprintf(out, "%-30s %-10s %-15s %-50s\n",
			lastSeen, event.Type, event.Reason, event.Message)
	}

	return nil
}

func (o *options) vmConsoleURL(cmd *cobra.Command, args []string) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	vm := &infrav1beta1.VirtualMachine{}
	key := types.NamespacedName{Namespace: o.namespace, Name: args[0]}
	if err := c.Get(ctx, key, vm); err != nil {
		return fmt.Errorf("failed to get VM %s: %w", key, err)
	}

	out := cmd.OutOrStdout()
	if vm.Status.ConsoleURL == "" {
		fmt.Fprintf(out, "No console URL available for VM %s\n", args[0])
	} else {
		fmt.Fprintf(out, "%s\n", vm.Status.ConsoleURL)
	}

	return nil
}

func (o *options) listProviders(cmd *cobra.Command, _ []string) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	providerList := &infrav1beta1.ProviderList{}
	if err := c.List(ctx, providerList, client.InNamespace(o.listNamespace(cmd))); err != nil {
		return fmt.Errorf("failed to list providers: %w", err)
	}

	out := cmd.OutOrStdout()
	if o.output != outputTable {
		return o.outputResource(out, c, providerList)
	}

	fmt.Fprintf(out, "%-20s %-20s %-15s %-30s %-10s\n", "NAMESPACE", "NAME", "TYPE", "ENDPOINT", "AGE")
	for i := range providerList.Items {
		provider := &providerList.Items[i]
		age := time.Since(provider.CreationTimestamp.Time).Truncate(time.Second)
		endpoint := provider.Spec.Endpoint
		if endpoint == "" {
			endpoint = noneValue
		}
		fmt.Fprintf(out, "%-20s %-20s %-15s %-30s %-10s\n",
			provider.Namespace, provider.Name, provider.Spec.Type, endpoint, age)
	}

	return nil
}

func (o *options) providerStatus(cmd *cobra.Command, args []string) error {
	c, err := o.getClient()
	if err != nil {
		return err
	}

	ctx, cancel := o.newContext(cmd)
	defer cancel()

	provider := &infrav1beta1.Provider{}
	key := types.NamespacedName{Namespace: o.namespace, Name: args[0]}
	if err := c.Get(ctx, key, provider); err != nil {
		return fmt.Errorf("failed to get provider %s: %w", key, err)
	}

	out := cmd.OutOrStdout()
	if o.output != outputTable {
		return o.outputResource(out, c, provider)
	}

	fmt.Fprintf(out, "Provider: %s\n", provider.Name)
	fmt.Fprintf(out, "Type: %s\n", provider.Spec.Type)
	fmt.Fprintf(out, "Endpoint: %s\n", provider.Spec.Endpoint)
	fmt.Fprintf(out, "Healthy: %t\n", provider.Status.Healthy)

	if len(provider.Status.Conditions) > 0 {
		fmt.Fprintf(out, "\nConditions:\n")
		for _, condition := range provider.Status.Conditions {
			fmt.Fprintf(out, "  %s: %s (%s)\n", condition.Type, condition.Status, condition.Reason)
		}
	}

	fmt.Fprintf(out, "\nNote: Use kubectl describe for full provider details\n")

	return nil
}

// getClient returns a client that knows the VirtRigaud API types.
func (o *options) getClient() (client.Client, error) {
	cfg, err := cliclient.RESTConfig(o.kubeconfig)
	if err != nil {
		return nil, err
	}
	return cliclient.NewClient(cfg)
}

// getClientset returns a typed client-go clientset.
func (o *options) getClientset() (kubernetes.Interface, error) {
	cfg, err := cliclient.RESTConfig(o.kubeconfig)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes clientset: %w", err)
	}
	return clientset, nil
}

// outputResource writes obj, a typed VirtRigaud object or list, to out in the
// --output format (json or yaml), with apiVersion and kind filled in.
func (o *options) outputResource(out io.Writer, c client.Client, obj runtime.Object) error {
	if err := setGVK(c, obj); err != nil {
		return err
	}

	var data []byte
	var err error
	switch o.output {
	case outputJSON:
		data, err = json.MarshalIndent(obj, "", "  ")
		if err == nil {
			data = append(data, '\n')
		}
	case outputYAML:
		data, err = yaml.Marshal(obj)
	default:
		return fmt.Errorf("unsupported output format %q (use table, json or yaml)", o.output)
	}
	if err != nil {
		return fmt.Errorf("encode %s output: %w", o.output, err)
	}

	if _, err := out.Write(data); err != nil {
		return fmt.Errorf("write %s output: %w", o.output, err)
	}
	return nil
}

// setGVK fills in apiVersion and kind on obj and, for a list, on each item.
// Typed reads leave them empty.
func setGVK(c client.Client, obj runtime.Object) error {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return fmt.Errorf("resolve kind of %T: %w", obj, err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	if !meta.IsListType(obj) {
		return nil
	}
	return meta.EachListItem(obj, func(item runtime.Object) error {
		itemGVK, err := apiutil.GVKForObject(item, c.Scheme())
		if err != nil {
			return fmt.Errorf("resolve kind of %T: %w", item, err)
		}
		item.GetObjectKind().SetGroupVersionKind(itemGVK)
		return nil
	})
}
