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

// Command driver is Spike 1B's load driver and invariant auditor
// (docs/performance/spike-1b-control-plane-baseline.md). Test-only.
//
// Subcommands:
//
//	setup     Providers (fake image), HostPools, Hosts, tenant namespaces, VMClasses
//	populate  seeded VirtualMachines with a steady-state status (manager scaled to 0)
//	burst     create N VMs at once and time each lifecycle transition
//	churn     create and delete VMs at a fixed rate; track backlog and recovery
//	hosts     sample Host heartbeat ages
//	status    count VMs by Ready / Placed reason
//	audit     compare API placement state with the fake provider's executions
//	cleanup   delete the VMs of one run
//	fakeconfig read or change a fake provider's latency and fault settings
//
// It talks to the cluster of --context with its own client (high QPS: the
// driver must not be the bottleneck it measures).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Fixed names of the experiment's objects.
const (
	infraNamespace = "spike-infra"
	credsSecret    = "fake-agent-creds" // #nosec G101 -- the name of the fake provider's Secret, not a credential
	poolName       = "pool"
	labelRun       = "spike1b/run"
	driverQPS      = 3000
	driverBurst    = 6000
)

// env is what every subcommand needs.
type env struct {
	cfg    *rest.Config
	c      client.Client
	scheme *runtime.Scheme
}

// newEnv connects to kubeContext.
func newEnv(kubeContext string) (*env, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig context %q: %w", kubeContext, err)
	}
	cfg.QPS, cfg.Burst = driverQPS, driverBurst
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	return &env{cfg: cfg, c: c, scheme: scheme}, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: driver <setup|populate|burst|churn|hosts|status|audit|cleanup|fakeconfig> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	kubeContext := fs.String("context", "kind-virtrigaud-spike1b", "kubeconfig context (never the current one by default)")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var run func(ctx context.Context, e *env) error
	switch cmd {
	case "setup":
		run = setupCmd(fs)
	case "populate":
		run = populateCmd(fs)
	case "burst":
		run = burstCmd(fs)
	case "churn":
		run = churnCmd(fs)
	case "hosts":
		run = hostsCmd(fs)
	case "status":
		run = statusCmd(fs)
	case "audit":
		run = auditCmd(fs)
	case "cleanup":
		run = cleanupCmd(fs)
	case "fakeconfig":
		run = fakeConfigCmd(fs)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", cmd)
		os.Exit(2)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	e, err := newEnv(*kubeContext)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(ctx, e); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
