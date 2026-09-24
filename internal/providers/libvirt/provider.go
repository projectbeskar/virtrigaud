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
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Provider implements the contracts.Provider interface for Libvirt/KVM via virsh
type Provider struct {
	// provider configuration
	config *v1beta1.Provider

	// Kubernetes client for reading secrets
	k8sClient client.Client

	// Virsh-based provider (replaces libvirt-go). Retained as the handle the
	// package's own ~138 runVirshCommand call sites operate on; the gRPC Server
	// reaches it through the registry seam instead (ADR-0008 PR 2).
	virshProvider *VirshProvider

	// registry owns the per-host connection(s). In single-host mode it holds
	// exactly one Conn (built from PROVIDER_ENDPOINT, wrapping virshProvider); in
	// clustered mode (ADR-0007 D3) it is the *hostconn.ClusterRegistry below,
	// holding N host-keyed connections projected from the mounted inventory.
	registry hostconn.Registry

	// hostID identifies the single host in single-host mode (equals the URI
	// authority). It is empty in clustered mode, where connections are addressed
	// by Host id via the registry, not through this single field.
	hostID hostconn.HostID

	// clusterReg is the N-host registry in CLUSTERED mode (ADR-0007 D3), nil in
	// single-host mode. It is the same object as registry (a ClusterRegistry
	// satisfies hostconn.Registry); the concrete handle is retained for the
	// watcher and for shutdown draining.
	clusterReg *hostconn.ClusterRegistry

	// watcher hot-reloads clusterReg from the mounted inventory file in CLUSTERED
	// mode (nil in single-host mode, and nil in clustered mode if the file-watch
	// could not be established — the provider then serves the initially-loaded
	// hosts without hot-reload). Stopped by Close before the registry is drained.
	watcher *hostconn.Watcher

	// cached credentials
	credentials *Credentials

	// --- ADR-0008 PR 4b shadow-compare reads (see shadow.go / native_flag.go) ---

	// nativeCfg is the parsed VIRTRIGAUD_LIBVIRT_NATIVE per-family driver flag (D4).
	// Empty/unset => every family off => pure virsh, zero behavior change.
	nativeCfg nativeConfig

	// shadowSampler bounds how often a shadowed read is actually shadowed
	// (VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE; default: every call).
	shadowSampler *sampler

	// shadowTimeoutValue bounds one shadow Describe's detached goroutine
	// (VIRTRIGAUD_LIBVIRT_SHADOW_TIMEOUT; default: shadowDescribeDefaultTimeout).
	shadowTimeoutValue time.Duration

	// describeNativeFn produces the go-libvirt shadow DescribeResponse on the
	// connection the virsh read ran on. It defaults to (*Provider).describeNative
	// (the real go-libvirt path) and is a struct field so unit tests can script
	// native results/errors/panics without a live libvirtd.
	describeNativeFn func(ctx context.Context, c libvirtConn, id string) (contracts.DescribeResponse, error)

	// listNativeFn produces the go-libvirt shadow VMInfo list (ADR-0008 PR 4c). It
	// defaults to (*Provider).listNative (the real go-libvirt path) and is a struct
	// field, mirroring describeNativeFn, so unit tests can script native
	// results/errors/panics without a live libvirtd.
	listNativeFn func(ctx context.Context) ([]contracts.VMInfo, error)

	// createOnHostFn runs the clustered create pipeline over a single leased host
	// connection (ADR-0007 P1). nil means "use the default",
	// (*Provider).createOnLeasedHost (narrow the lease to its *virshConn, then run
	// the shared createVM core). It is a struct field, mirroring describeNativeFn/
	// listNativeFn, so clustered-routing tests can inject a recorder that asserts
	// host selection and lease release without a live libvirtd.
	createOnHostFn func(ctx context.Context, lease hostconn.Conn, req contracts.CreateRequest) (contracts.CreateResponse, error)

	// shadowWG tracks in-flight detached shadow goroutines so tests (and a future
	// graceful shutdown) can drain them; each is independently time-bounded.
	shadowWG sync.WaitGroup

	// logger is the provider's structured logger (defaults to slog.Default()).
	logger *slog.Logger

	// imageDirs are the allowed image directories (VIRTRIGAUD_LIBVIRT_IMAGE_DIRS,
	// validated at construction). Empty means "not loaded": imagePolicy then
	// reads the environment on use, so struct-literal providers (tests) still
	// get the default policy rather than an unchecked one.
	imageDirs []string

	// hostStagingDir is the host directory per-create staging files (domain
	// XML, cloud-init seeds) are made in. Empty means defaultHostStagingDir;
	// tests point it at a scratch directory (see staging.go).
	hostStagingDir string
}

// imagePolicy returns the provider's image-path confinement policy (see
// imagepath.go). An unset configuration yields the DefaultImageDir policy; it
// never returns a policy that skips the checks.
func (p *Provider) imagePolicy() (imagePathPolicy, error) {
	if len(p.imageDirs) > 0 {
		return imagePathPolicy{dirs: p.imageDirs}, nil
	}
	dirs, err := imageDirsFromEnv()
	if err != nil {
		return imagePathPolicy{}, contracts.NewRetryableError("load image directory policy", err)
	}
	return imagePathPolicy{dirs: dirs}, nil
}

// loadImageDirs validates and records the allowed image directories from the
// environment. Constructors call it so a malformed VIRTRIGAUD_LIBVIRT_IMAGE_DIRS
// fails provider start-up (fail closed) instead of surfacing per request.
func (p *Provider) loadImageDirs() error {
	dirs, err := imageDirsFromEnv()
	if err != nil {
		return err
	}
	p.imageDirs = dirs
	slog.Info("libvirt image path confinement configured", "allowed_image_dirs", dirs)
	return nil
}

// ProviderConfig represents the configuration for the provider
type ProviderConfig struct {
	Spec      ProviderSpec
	Namespace string
}

// ProviderSpec represents the spec of the provider configuration
type ProviderSpec struct {
	Endpoint            string
	CredentialSecretRef CredentialSecretRef
}

// CredentialSecretRef represents a reference to a credential secret
type CredentialSecretRef struct {
	Name      string
	Namespace string
}

// Credentials holds Libvirt authentication information
type Credentials struct {
	Username string
	Password string
	// SSH key authentication
	SSHPrivateKey string
	SSHPublicKey  string
	// For TLS connections
	CertData string
	KeyData  string
	CAData   string
}

const (
	// CredentialsPath is where the controller mounts the credentials secret
	CredentialsPath = "/etc/virtrigaud/credentials"
)

// Config holds the libvirt provider configuration
type Config struct {
	Endpoint      string
	Username      string
	Password      string
	SSHPrivateKey string
}

// New creates a new Libvirt provider that reads configuration from environment
// and mounted secrets.
//
// It fails closed: if the virsh provider cannot initialize (bad/absent
// credentials, unreachable host), New returns an error instead of a Provider so
// the process never begins serving gRPC on a dead connection. This fixes the
// prior behaviour where an Initialize failure was logged and swallowed
// (provider.go:141), matching NewProvider and PR #291 finding B2. The caller
// (cmd/provider-libvirt/main.go) exits non-zero on error, so the pod restarts
// until the host is reachable rather than reporting healthy while every RPC
// fails.
func New() (*Provider, error) {
	// ADR-0007 D3/D9 mode detection: a mounted host-inventory file selects
	// CLUSTERED topology (N host-keyed connections, hot-reloaded from the file);
	// its ABSENCE keeps today's single-host path below byte-for-byte unchanged.
	// The provider reads the mounted file only — never the Kubernetes API (#297).
	if hostsFile := hostsFilePath(); clusteredModeEnabled(hostsFile) {
		return newClusteredProvider(hostsFile)
	}

	// Load configuration from environment (set by provider controller)
	config := &Config{
		Endpoint: os.Getenv("PROVIDER_ENDPOINT"),
	}

	// Credentials are now loaded by virsh provider from environment variables

	p := &Provider{
		config:    nil, // We'll create a minimal config
		k8sClient: nil, // No K8s client needed in container mode
		credentials: &Credentials{
			Username: config.Username,
			Password: config.Password,
		},
	}
	if err := p.loadImageDirs(); err != nil {
		return nil, err
	}

	// Try to establish libvirt connection
	slog.Info("Libvirt provider configuration loaded",
		"endpoint", config.Endpoint,
		"username", config.Username,
		"password_length", len(config.Password))

	// Create provider configuration
	providerConfig := &ProviderConfig{
		Spec: ProviderSpec{
			Endpoint: config.Endpoint,
			CredentialSecretRef: CredentialSecretRef{
				Name:      "libvirt-credentials", // Default name
				Namespace: "default",
			},
		},
		Namespace: "default",
	}

	// Create virsh provider to replace libvirt-go
	virshProvider := NewVirshProvider(providerConfig)
	p.virshProvider = virshProvider

	// Create minimal v1beta1.Provider config for compatibility
	p.config = &v1beta1.Provider{
		Spec: v1beta1.ProviderSpec{
			Endpoint: config.Endpoint,
		},
	}

	// Build the per-host connection seam (ADR-0008 PR 2). One host today, keyed
	// off PROVIDER_ENDPOINT; ADR-0007 P1 changes only this to project N hosts.
	hostID := hostIDFromEndpoint(config.Endpoint)
	registry, err := hostconn.NewRegistry(newVirshConn(hostID, virshProvider))
	if err != nil {
		return nil, fmt.Errorf("build libvirt host registry: %w", err)
	}
	p.registry = registry
	p.hostID = hostID

	// Initialize the virsh provider — fail closed (B2). Previously this logged
	// and continued, so the process could serve gRPC on a dead connection.
	ctx := context.Background()
	if err := virshProvider.Initialize(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to initialize virsh provider", err)
	}

	// Wire the ADR-0008 PR 4b shadow-compare reads (D4/D6). Off unless
	// VIRTRIGAUD_LIBVIRT_NATIVE opts a family in; when off this is pure virsh.
	p.initShadow(slog.Default())

	log.Printf("INFO Successfully initialized virsh provider")
	return p, nil
}

// newClusteredProvider builds the libvirt provider in CLUSTERED topology
// (ADR-0007 D3): it projects N host-keyed connections from the mounted
// inventory file and hot-reloads them when the file changes — lazy-open on
// host-add, graceful-drain on host-remove — without ever reading the Kubernetes
// API (#297).
//
// It fails-SAFE, not fails-closed: a malformed or unreadable inventory at
// startup is NOT fatal (that would crash the whole clustered provider on one
// bad render). It starts with an empty host set and lets the file-watcher
// reconcile once the file becomes valid. Connections are NOT dialed here — the
// dial waits for the first use of a host (lazy-open).
func newClusteredProvider(hostsFile string) (*Provider, error) {
	logger := slog.Default()
	logger.Info("libvirt provider starting in CLUSTERED topology (ADR-0007)", "hosts_file", hostsFile)

	inv, err := hostconn.LoadInventory(hostsFile)
	if err != nil {
		logger.Warn("libvirt clustered: initial host inventory unreadable; starting with an empty host set (watcher will reconcile when it becomes valid)",
			"hosts_file", hostsFile, "error", err.Error())
		inv = hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	}

	dialer := newClusterDialer(clusterKnownHostsDir(), logger)
	reg, err := hostconn.NewClusterRegistry(inv, dialer, logger)
	if err != nil {
		return nil, fmt.Errorf("build clustered host registry: %w", err)
	}

	p := &Provider{
		config:      &v1beta1.Provider{Spec: v1beta1.ProviderSpec{}},
		k8sClient:   nil, // no K8s client in container mode (#297)
		credentials: &Credentials{},
		registry:    reg,
		clusterReg:  reg,
		hostID:      "", // clustered: connections are addressed by Host id, not one hostID
		// virshProvider is an ALWAYS-FAILING handle (ADR-0007 Addendum A, A1): a
		// clustered provider has no single-host connection, so any call that was
		// not routed to a Host through withHostConn fails cleanly here — it can
		// never fall through to an empty-URI or local connection — and is counted
		// (unroutableHits) so tests prove no per-VM RPC reaches it.
		virshProvider: newUnroutableVirshProvider(),
	}
	// Operator configuration (not inventory), so fail closed on a malformed
	// value. The one policy is enforced on every host against that host's
	// filesystem (the confinement runs over the leased target-host connection).
	if err := p.loadImageDirs(); err != nil {
		return nil, err
	}
	p.initShadow(logger)

	// Start the file-watch / hot-reload. A watcher-setup failure is non-fatal:
	// serve the initially-loaded hosts without hot-reload rather than refuse to
	// start.
	if watcher, werr := hostconn.NewWatcher(hostsFile, reg, logger); werr != nil {
		logger.Warn("libvirt clustered: host-inventory watch could not start; serving without hot-reload",
			"hosts_file", hostsFile, "error", werr.Error())
	} else {
		watcher.Start()
		p.watcher = watcher
	}

	logger.Info("libvirt clustered provider initialized", "hosts", len(reg.Hosts()))
	return p, nil
}

// Close releases the provider's connection resources. In clustered mode it
// stops the hot-reload watcher FIRST (so no reconcile races the drain) and then
// drains/closes the registry; in single-host mode it closes the one connection.
// It is invoked on graceful shutdown (cmd/provider-libvirt/main.go) after the
// gRPC server has drained in-flight RPCs, so no in-flight operation is severed.
func (p *Provider) Close() error {
	if p.watcher != nil {
		_ = p.watcher.Close()
	}
	if p.registry != nil {
		return p.registry.Close()
	}
	return nil
}

// Removed old file-based credential loading - now using environment variables via virsh provider

// Removed old libvirt-go connection logic - now using virsh provider

// NewProvider creates a new Libvirt provider instance (legacy K8s API method)
func NewProvider(ctx context.Context, k8sClient client.Client, provider *v1beta1.Provider) (contracts.Provider, error) {
	if string(provider.Spec.Type) != "libvirt" {
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf("invalid provider type: %s, expected libvirt", string(provider.Spec.Type)), nil)
	}

	log.Printf("INFO Creating virsh-based provider from K8s API")

	// Create provider configuration for virsh
	providerConfig := &ProviderConfig{
		Spec: ProviderSpec{
			Endpoint: provider.Spec.Endpoint,
			CredentialSecretRef: CredentialSecretRef{
				Name:      provider.Spec.CredentialSecretRef.Name,
				Namespace: provider.Spec.CredentialSecretRef.Namespace,
			},
		},
		Namespace: provider.Namespace,
	}

	// Create virsh provider
	virshProvider := NewVirshProvider(providerConfig)

	p := &Provider{
		config:        provider,
		k8sClient:     k8sClient,
		virshProvider: virshProvider,
		credentials:   &Credentials{},
	}
	if err := p.loadImageDirs(); err != nil {
		return nil, contracts.NewInvalidSpecError("load image directory policy", err)
	}

	// Build the per-host connection seam (ADR-0008 PR 2), keyed off the
	// provider's endpoint, so this path is consistent with New().
	hostID := hostIDFromEndpoint(provider.Spec.Endpoint)
	registry, err := hostconn.NewRegistry(newVirshConn(hostID, virshProvider))
	if err != nil {
		return nil, contracts.NewRetryableError("build libvirt host registry", err)
	}
	p.registry = registry
	p.hostID = hostID

	// Initialize the virsh provider
	if err := virshProvider.Initialize(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to initialize virsh provider", err)
	}

	// Wire the ADR-0008 PR 4b shadow-compare reads (D4/D6), consistent with New().
	p.initShadow(slog.Default())

	log.Printf("INFO Successfully created virsh-based provider via K8s API")
	return p, nil
}

// conn returns the libvirt connection for the provider's single host, obtained
// from the registry seam (ADR-0008 PR 2) rather than the directly-held
// virshProvider field. The gRPC Server uses this for the snapshot / import / disk
// RPCs that previously type-asserted the concrete *Provider.
//
// It resolves the Conn through the Registry on every call (never caches it, per
// the ADR's connection-lifecycle rule) and narrows the transport-neutral
// hostconn.Conn to the richer libvirtConn view those RPCs need. A future
// non-virsh transport that does not provide the virsh helpers fails here cleanly
// rather than silently.
func (p *Provider) conn(ctx context.Context) (libvirtConn, error) {
	if p.registry == nil {
		return nil, contracts.NewRetryableError("libvirt provider not initialized", nil)
	}
	c, err := p.registry.ConnFor(ctx, p.hostID)
	if err != nil {
		return nil, fmt.Errorf("get libvirt host connection %q: %w", p.hostID, err)
	}
	lc, ok := c.(libvirtConn)
	if !ok {
		return nil, fmt.Errorf("libvirt host connection %q does not support virsh operations", p.hostID)
	}
	return lc, nil
}

// Compile-time proof that *Provider satisfies the backend interface the gRPC
// Server holds (server.go), so no concrete type assertion is needed there.
var _ providerBackend = (*Provider)(nil)

// clustered reports whether the provider runs in CLUSTERED topology (ADR-0007
// D3): a non-nil clusterReg means it fronts N host-keyed connections projected
// from the mounted inventory. Single-host mode (clusterReg nil) reports false,
// which keeps GetCapabilities.supports_clustering false and the host-inventory
// RPCs Unimplemented there (D7/D9).
func (p *Provider) clustered() bool {
	return p.clusterReg != nil
}

// Validate ensures the provider connection is healthy using virsh
func (p *Provider) Validate(ctx context.Context) error {
	// CLUSTERED mode (ADR-0007 D3): there is no single PROVIDER_ENDPOINT to probe
	// — the provider fronts N hosts addressed by id, each dialed lazily from the
	// mounted inventory. The single-host `virsh -c <endpoint> list` below would
	// run with an EMPTY endpoint here (`virsh -c "" list`), fail "cannot connect
	// to the hypervisor", and falsely mark the whole provider not-ready before the
	// manager ever calls GetHostInfo. Mirror #317's "legacy single-host dispatch
	// is inert in clustered mode": validate the CLUSTERED setup (registry
	// initialized with at least one host loaded from the inventory) and return
	// success. Per-host reachability is reported through ListHosts/GetHostInfo —
	// which mark an unreachable host NotReady without failing the whole provider —
	// not this global readiness probe.
	if p.clustered() {
		return p.validateClustered()
	}

	if p.virshProvider == nil {
		return contracts.NewRetryableError("virsh provider not initialized", nil)
	}

	// Validate must stay lightweight: controllers call it before most provider
	// operations. Do not use listDomains here; it runs domstate for every domain
	// and can exceed the manager-side gRPC deadline on busy libvirt hosts.
	result, err := p.virshProvider.runVirshCommand(ctx, "list", "--all", "--name")
	if err != nil {
		return contracts.NewRetryableError("virsh connection validation failed", err)
	}

	domainCount := 0
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			domainCount++
		}
	}

	log.Printf("INFO Connection validation successful - found %d domains", domainCount)
	return nil
}

// validateClustered is the CLUSTERED-mode (ADR-0007 D3) readiness check Validate
// uses in place of the single-host `virsh list` probe. It confirms the N-host
// connection registry is initialized and fronts at least one host projected from
// the mounted inventory; it deliberately does NOT dial any host or run virsh (a
// clustered provider has no single endpoint, and per-host reachability is
// reported through ListHosts/GetHostInfo, which mark an unreachable host
// NotReady without failing the whole provider).
//
// A registry that is not yet initialized, or that currently fronts zero hosts
// (e.g. a malformed inventory the watcher has not reconciled yet), is reported as
// a RETRYABLE not-ready so the manager re-checks once the inventory is valid —
// never a hard failure that would take a clustered provider down over one bad
// render (D9 fail-safe).
func (p *Provider) validateClustered() error {
	if p.clusterReg == nil {
		return contracts.NewRetryableError("clustered libvirt provider registry not initialized", nil)
	}
	hosts := p.clusterReg.Hosts()
	if len(hosts) == 0 {
		return contracts.NewRetryableError("clustered libvirt provider fronts no hosts yet (mounted inventory empty or not yet reconciled)", nil)
	}
	log.Printf("INFO Clustered connection validation successful - registry fronts %d host(s)", len(hosts))
	return nil
}

// Contract methods are now implemented in provider_virsh.go using virsh commands
