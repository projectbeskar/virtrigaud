//go:build virtrigaud_dev

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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/cloudhypervisor"
	"github.com/projectbeskar/virtrigaud/internal/version"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

// exitUsage is the exit status of a usage error.
const exitUsage = 2

// envProviderNamespace is the environment variable the manager sets to the
// Provider's namespace (the downward API); it defaults
// --hosts-secret-namespace.
const envProviderNamespace = "PROVIDER_NAMESPACE"

// minInventoryFailClosedAfter is the shortest --inventory-fail-closed-after
// accepted: two inventory reload intervals.
const minInventoryFailClosedAfter = 2 * cloudhypervisor.DefaultInventoryReloadInterval

// serviceAccountNamespaceFile is where Kubernetes mounts the pod's namespace
// with its ServiceAccount token.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// flags are the command's settings.
type flags struct {
	port, healthPort      int
	hostsFile             string
	hostsFileSet          bool
	hostsSecret           string
	hostsSecretNamespace  string
	failClosedAfter       time.Duration
	clientCert, clientKey string
	providerUID           string
}

// parseFlags parses args.
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	var f flags
	fs := flag.NewFlagSet("provider-cloudhypervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&f.port, "port", 9443, "gRPC port the manager connects to")
	fs.IntVar(&f.healthPort, "health-port", 8080, "HTTP health port")
	fs.StringVar(&f.hostsFile, "hosts-file", filepath.Join(hostsecret.MountPath, hostsecret.SecretDataKey), "the mounted host inventory")
	fs.StringVar(&f.hostsSecret, "hosts-secret", "", "name of the host inventory Secret to follow through the Kubernetes API instead of --hosts-file "+
		"(needs get and watch on that Secret only)")
	fs.DurationVar(&f.failClosedAfter, "inventory-fail-closed-after", cloudhypervisor.DefaultInventoryFailClosedAfter,
		"with --hosts-secret: refuse every host once no valid inventory was applied for this long (at least "+minInventoryFailClosedAfter.String()+")")
	fs.StringVar(&f.hostsSecretNamespace, "hosts-secret-namespace", "", "namespace of --hosts-secret; defaults to $"+envProviderNamespace+
		", then to the pod's namespace")
	fs.StringVar(&f.clientCert, "agent-client-cert", "", "PEM certificate the Provider presents to agents (required)")
	fs.StringVar(&f.clientKey, "agent-client-key", "", "PEM key of --agent-client-cert (required)")
	fs.StringVar(&f.providerUID, "provider-uid", "", "UID of the Provider resource; agents enrolled with another Provider are refused")
	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	if fs.NArg() != 0 {
		return flags{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "hosts-file" {
			f.hostsFileSet = true
		}
	})
	if f.hostsSecret != "" && f.hostsFileSet {
		return flags{}, errors.New("--hosts-file and --hosts-secret are mutually exclusive")
	}
	if f.hostsSecret == "" && f.hostsSecretNamespace != "" {
		return flags{}, errors.New("--hosts-secret-namespace needs --hosts-secret")
	}
	if f.failClosedAfter < minInventoryFailClosedAfter {
		return flags{}, fmt.Errorf("--inventory-fail-closed-after must be at least %s", minInventoryFailClosedAfter)
	}
	if f.clientCert == "" || f.clientKey == "" {
		return flags{}, errors.New("--agent-client-cert and --agent-client-key are required")
	}
	return f, nil
}

// realMain runs the Provider until it is told to stop.
func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintf(stdout, "virtrigaud-provider-cloudhypervisor %s (development build)\n", version.String())
		return exitOK
	}
	f, err := parseFlags(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := run(context.Background(), f, log); err != nil {
		log.Error("provider-cloudhypervisor stopped", "error", err.Error())
		return exitError
	}
	return exitOK
}

// run serves the Provider.
func run(ctx context.Context, f flags, log *slog.Logger) error {
	if f.providerUID == "" {
		log.Warn("--provider-uid is not set: an agent enrolled with another Provider is not refused by this Provider " +
			"(the agent still refuses this Provider); a deployment rendered by the manager must set it")
	}
	client, err := tls.LoadX509KeyPair(f.clientCert, f.clientKey)
	if err != nil {
		return fmt.Errorf("load the agent client certificate: %w", err)
	}
	tlsResolution, err := server.ResolveTLSAndAuth()
	switch {
	case err == nil:
	case errors.Is(err, server.ErrInsecureModeOptedIn):
		log.Warn("serving the manager in plaintext: the insecure opt-in is set and no TLS material is mounted")
	default:
		return fmt.Errorf("resolve the manager TLS: %w", err)
	}
	opts := cloudhypervisor.Options{ClientCertificate: client, ProviderUID: f.providerUID, Logger: log,
		InventoryFailClosedAfter: f.failClosedAfter}
	var p *cloudhypervisor.Provider
	if f.hostsSecret != "" {
		p, err = newFromSecret(ctx, f, opts, log)
	} else {
		p, err = cloudhypervisor.NewFromFile(f.hostsFile, opts)
	}
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	cfg := server.DefaultConfig()
	cfg.Port = f.port
	cfg.HealthPort = f.healthPort
	cfg.Logger = log
	cfg.TLS = tlsResolution.TLS
	cfg.MetricsHandler = promhttp.HandlerFor(obsmetrics.GetRegistry(), promhttp.HandlerOpts{})
	cfg.Middleware = &middleware.Config{
		Logging:  &middleware.LoggingConfig{Enabled: true, Logger: log},
		Recovery: &middleware.RecoveryConfig{Enabled: true, Logger: log},
		Auth:     tlsResolution.Auth,
	}
	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("create the gRPC server: %w", err)
	}
	p.Register(srv)
	log.Info("serving the clustered Provider for per-host agents (development build)",
		"version", version.String(), "port", f.port, "agentAPIVersion", cloudhypervisor.APIVersion)
	return srv.Serve(ctx)
}

// newFromSecret returns a Provider that follows the inventory Secret through
// the API, with the pod's in-cluster credentials.
func newFromSecret(ctx context.Context, f flags, opts cloudhypervisor.Options, log *slog.Logger) (*cloudhypervisor.Provider, error) {
	ns, err := secretNamespace(f.hostsSecretNamespace, os.Getenv(envProviderNamespace), serviceAccountNamespaceFile)
	if err != nil {
		return nil, err
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("--hosts-secret needs the pod's Kubernetes API credentials: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create the Kubernetes client: %w", err)
	}
	log.Info("following the host inventory Secret through the Kubernetes API", "namespace", ns, "secret", f.hostsSecret)
	return cloudhypervisor.NewFromSecret(ctx, cs.CoreV1().Secrets(ns), f.hostsSecret, opts)
}

// secretNamespace returns the namespace of the inventory Secret: flag, then
// env, then the content of the pod's namespace file.
func secretNamespace(flagValue, envValue, nsFile string) (string, error) {
	for _, v := range []string{flagValue, envValue} {
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	b, err := os.ReadFile(filepath.Clean(nsFile))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("the namespace of --hosts-secret is unknown: set --hosts-secret-namespace or $" + envProviderNamespace)
	}
	return strings.TrimSpace(string(b)), nil
}
