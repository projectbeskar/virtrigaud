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
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

// providerServiceName is the fully-qualified gRPC service the libvirt provider
// must expose. Before ADR-0003 PR-2 this was registered on a raw
// grpc.NewServer() via providerv1.RegisterProviderServer; after the migration
// it is registered through the SDK server's RegisterProvider. Either path must
// land the same service.
const providerServiceName = "provider.v1.Provider"

// TestLibvirtServer_ImplementsProviderServer is a compile-time assertion that
// the libvirt Server still satisfies the generated providerv1.ProviderServer
// interface — the exact contract the raw RegisterProviderServer enforced.
// If the SDK migration ever drifts the type, this fails to compile.
func TestLibvirtServer_ImplementsProviderServer(t *testing.T) {
	var _ providerv1.ProviderServer = libvirt.NewServer(nil)
}

// TestLibvirtServer_RegistersOnSDKServer proves the SDK-based server (the
// replacement for the raw grpc.NewServer in cmd/provider-libvirt/main.go)
// boots and ends up serving the same provider.v1.Provider service the raw
// server did. This is the load-bearing behavior-preservation check for the
// ADR-0003 PR-2 libvirt framework swap.
//
// The provider impl is nil here on purpose: NewServer only stores the
// reference, and registration cares about the service descriptor, not the
// backing implementation. Using nil keeps the test hermetic (no virsh exec,
// no environment dependency).
func TestLibvirtServer_RegistersOnSDKServer(t *testing.T) {
	cfg := server.DefaultConfig()
	// Bind to :0-equivalent by not calling Serve; we only construct + register.
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	srv.RegisterProvider(libvirt.NewServer(nil))

	info := srv.GetServiceInfo()
	if _, ok := info[providerServiceName]; !ok {
		names := make([]string, 0, len(info))
		for name := range info {
			names = append(names, name)
		}
		t.Fatalf("expected service %q to be registered, got services: %v",
			providerServiceName, names)
	}
}

// TestLibvirtServer_RegistersWithTLSConfig mirrors the TLS-enabled startup
// path: a server constructed with a TLS-less config still registers the
// provider service (the TLS material itself is exercised by the server
// package's buildtls tests; here we only confirm the migration's
// construction → registration flow is intact).
func TestLibvirtServer_RegistersWithTLSConfig(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.Port = 0
	cfg.HealthPort = 0
	cfg.TLS = nil // plaintext-mode equivalent; TLS field assertions live in buildtls_test.go

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New with nil TLS: %v", err)
	}
	srv.RegisterProvider(libvirt.NewServer(nil))

	if _, ok := srv.GetServiceInfo()[providerServiceName]; !ok {
		t.Fatalf("expected %q registered on plaintext server", providerServiceName)
	}
}
