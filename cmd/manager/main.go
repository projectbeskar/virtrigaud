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
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/controller"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/version"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Set up logger early to avoid "log.SetLogger(...) was never called" warnings
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(infrav1beta1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// versionString returns the single-line banner printed by `--version`.
// Extracted from main() so it can be unit-tested without spawning a
// subprocess (the surrounding os.Exit(0) path is not directly testable).
// Format: "virtrigaud-manager <version-string>" where the version-string
// comes from internal/version.String() (e.g. "v0.3.6 (gitSHA=abc123 go=go1.25.10)"
// in release builds, or "dev (gitSHA=unknown ...)" in unstamped builds).
//
// Ported from cmd/main.go (H1 PR-1 / #114) as part of the build-path
// consolidation (ADR-0002).
func versionString() string {
	return fmt.Sprintf("virtrigaud-manager %s", version.String())
}

// nolint:gocyclo
func main() {
	// Handle --version flag before any other flag parsing, mirroring
	// cmd/main.go behaviour. Using os.Args lookup (not flag.BoolVar +
	// flag.Parse) means `--version` works even when invalid flags
	// follow, and avoids the long help text that flag.Parse prints on
	// error. Ported from cmd/main.go (H1 PR-1 / #114, ADR-0002).
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(versionString())
		os.Exit(0)
	}

	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var enforceProviderCapabilities bool
	var migrationStorageAllowedHosts string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely (HTTPS). "+
			"When true, the metrics endpoint also enforces RBAC authn/authz "+
			"via controller-runtime's filters.WithAuthenticationAndAuthorization.")
	// Certificate-rotation flags (H1 PR-1 / #114). All default to empty —
	// when both --webhook-cert-path / --metrics-cert-path are empty the
	// behaviour matches the pre-PR canonical manager exactly (no
	// certwatcher initialised; controller-runtime falls back to its
	// self-signed default for the metrics endpoint).
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate. "+
			"When non-empty, the manager hot-reloads the cert via certwatcher "+
			"so cert-manager renewals don't require a pod restart.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	// Capability enforcement (issue #176). OFF by default: when off,
	// snapshot/migration behaviour is byte-for-byte unchanged. When on, the
	// snapshot and migration controllers gate capability-dependent
	// operations on the provider's self-reported capabilities (queried live
	// over gRPC) and refuse operations a provider declares it does not
	// support, instead of letting the RPC fail downstream. Opt-in because a
	// provider that UNDER-reports a capability would otherwise block
	// operations it can actually perform — operators must confirm their
	// providers' capability flags are accurate before enabling this.
	flag.BoolVar(&enforceProviderCapabilities, "enforce-provider-capabilities", false,
		"If set, gate snapshot and migration operations on the provider's "+
			"self-reported capabilities (issue #176). Opt-in: ensure provider "+
			"capability flags are accurate before enabling, as under-reported "+
			"capabilities will block otherwise-supported operations.")
	flag.StringVar(&migrationStorageAllowedHosts, "migration-storage-allowed-hosts", "",
		"Comma-separated IP/CIDR allowlist for migration staging backends (the S3 "+
			"endpoint host and NFS server). Empty = permissive except the always-denied "+
			"loopback/link-local/metadata/multicast targets (ADR-0006 C3, SSRF gate). "+
			"Set to lock migration egress to known storage networks.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Override the logger with flag-based options if provided
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Emit a virtrigaud_build_info{version,git_sha,go_version,component} sample
	// so the manager's /metrics endpoint exposes a virtrigaud_* family at startup.
	// Without this call the build_info GaugeVec stays empty and the family does
	// not appear in /metrics output, defeating Prometheus-based release verification.
	metrics.SetupMetrics(version.Version, version.GitSHA, metrics.ComponentManager)
	setupLog.Info("virtrigaud metrics registered",
		"version", version.Version,
		"gitSHA", version.GitSHA,
		"component", metrics.ComponentManager)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Certificate watchers for hot rotation (H1 PR-1 / #114). When the
	// corresponding *--*-cert-path flag is empty, the watcher stays nil
	// and downstream code paths (mgr.Add at the bottom, the GetCertificate
	// callback wiring above the manager) are skipped — behaviour is
	// identical to the pre-PR canonical manager.
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options (inherits the http/2-disable hook from
	// tlsOpts; appends certwatcher GetCertificate when the flag is set).
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics server options. Built as a variable (not inline in
	// ctrl.Options) so we can layer on the RBAC FilterProvider and the
	// metrics certwatcher conditionally below — neither activates at
	// defaults (--metrics-secure=false, --metrics-cert-path="").
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider protects /metrics with authn/authz: only
		// ServiceAccounts with `get` on the `/metrics` nonResourceURL
		// can scrape. Activates ONLY when --metrics-secure=true, which
		// currently defaults to false — so default deployments see no
		// behaviour change. The PR-5 default-flip to secure=true will
		// make this the on-by-default posture (held for v0.4.0 per
		// ADR-0002).
		// See: https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// Configure client with rate limiting to prevent API server overload
	// These settings prevent reconciliation storms from overwhelming etcd/API server
	// Conservative settings for cluster stability
	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = 20   // Max 20 queries per second to API server
	restConfig.Burst = 40 // Allow bursts up to 40 requests

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "8da97394.virtrigaud.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create the per-Provider CircuitBreaker registry shared across all
	// gRPC clients (G6 / #111). DefaultConfig: FailureThreshold=10,
	// ResetTimeout=60s, HalfOpenMaxCalls=3. The registry allocates one
	// breaker per Provider CR; metric series with provider_type +
	// provider labels emit on /metrics for each.
	cbRegistry := resilience.NewRegistry(resilience.DefaultConfig())

	// Create remote provider resolver (all providers are now remote)
	remoteResolver := remote.NewResolver(mgr.GetClient(), cbRegistry)

	if err = (&controller.VirtualMachineReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		RemoteResolver: remoteResolver,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualMachine")
		os.Exit(1)
	}
	if err = (&controller.ProviderReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		RemoteResolver: remoteResolver,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Provider")
		os.Exit(1)
	}
	if err = (&controller.VMClassReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMClass")
		os.Exit(1)
	}
	if err = (&controller.VMImageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMImage")
		os.Exit(1)
	}
	if err = (&controller.VMNetworkAttachmentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMNetworkAttachment")
		os.Exit(1)
	}

	// Register VMSnapshot controller
	vmsnapshotReconciler := controller.NewVMSnapshotReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		remoteResolver,
		mgr.GetEventRecorderFor("vmsnapshot-controller"),
		enforceProviderCapabilities,
	)
	if err = vmsnapshotReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMSnapshot")
		os.Exit(1)
	}

	// Register VMMigration controller
	vmmigrationReconciler := controller.NewVMMigrationReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		remoteResolver,
		mgr.GetEventRecorderFor("vmmigration-controller"),
		enforceProviderCapabilities,
	)
	storageHostPolicy, hpErr := storagemigration.NewHostPolicy(strings.Split(migrationStorageAllowedHosts, ","))
	if hpErr != nil {
		setupLog.Error(hpErr, "invalid --migration-storage-allowed-hosts")
		os.Exit(1)
	}
	vmmigrationReconciler.StorageHostPolicy = storageHostPolicy
	if err = vmmigrationReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMMigration")
		os.Exit(1)
	}

	// Register VMAdoption controller
	if err = (&controller.VMAdoptionReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		RemoteResolver: remoteResolver,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMAdoption")
		os.Exit(1)
	}

	// Register VMClone controller (MVP: vmRef source, same-provider, full/linked)
	vmcloneReconciler := controller.NewVMCloneReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		remoteResolver,
		mgr.GetEventRecorderFor("vmclone-controller"),
	)
	if err = vmcloneReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMClone")
		os.Exit(1)
	}

	// Register VMSet controller (not-yet-active stub; reports
	// ControllerNotImplemented condition only)
	if err = (&controller.VMSetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VMSet")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	// Register cert watchers with the manager so they run as Runnables
	// alongside the controllers (H1 PR-1 / #114). nil-guarded so default
	// deployments (no cert paths set) have zero overhead.
	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}
	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
