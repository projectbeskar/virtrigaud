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
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infravirtrigaudiov1alpha1 "github.com/projectbeskar/virtrigaud/api/v1alpha1"
	"github.com/projectbeskar/virtrigaud/internal/providers/vsphere"
	providerv1 "github.com/projectbeskar/virtrigaud/internal/rpc/provider/v1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = infravirtrigaudiov1alpha1.AddToScheme(scheme)
}

func main() {
	var (
		grpcAddr         = flag.String("grpc-addr", ":9443", "gRPC server address")
		metricsAddr      = flag.String("metrics-addr", ":8080", "Metrics server address")
		tlsEnabled       = flag.Bool("tls-enabled", false, "Enable TLS for gRPC server")
		tlsDir           = flag.String("tls-dir", "/etc/virtrigaud/tls", "TLS certificates directory")
		providerType     = flag.String("provider-type", "vsphere", "Provider type")
		providerEndpoint = flag.String("provider-endpoint", "", "Provider endpoint")
		_                = flag.String("creds-dir", "/etc/virtrigaud/credentials", "Credentials directory")
	)

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("Starting vSphere provider server",
		"grpc-addr", *grpcAddr,
		"metrics-addr", *metricsAddr,
		"tls-enabled", *tlsEnabled,
		"provider-type", *providerType,
		"provider-endpoint", *providerEndpoint,
	)

	// Create Kubernetes client for reading secrets
	config, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes client")
		os.Exit(1)
	}

	// Create a fake Provider object for the vSphere provider
	// In reality, this would be loaded from the environment or passed down
	providerObj := &infravirtrigaudiov1alpha1.Provider{
		Spec: infravirtrigaudiov1alpha1.ProviderSpec{
			Type:     *providerType,
			Endpoint: *providerEndpoint,
			CredentialSecretRef: infravirtrigaudiov1alpha1.ObjectRef{
				Name: "provider-creds", // This would come from env
			},
		},
	}

	// Set namespace for the provider (would come from environment)
	providerObj.Namespace = os.Getenv("PROVIDER_NAMESPACE")
	if providerObj.Namespace == "" {
		providerObj.Namespace = "default"
	}

	// Create the vSphere provider instance
	ctx := context.Background()
	provider, err := vsphere.NewProvider(ctx, k8sClient, providerObj)
	if err != nil {
		setupLog.Error(err, "unable to create vSphere provider")
		os.Exit(1)
	}

	// Create gRPC server
	server := vsphere.NewServer(provider)

	// Setup gRPC server with optional TLS
	var grpcServer *grpc.Server
	if *tlsEnabled {
		grpcServer, err = createTLSGRPCServer(*tlsDir)
		if err != nil {
			setupLog.Error(err, "unable to create TLS gRPC server")
			os.Exit(1)
		}
	} else {
		grpcServer = grpc.NewServer()
	}

	// Register services
	providerv1.RegisterProviderServer(grpcServer, server)

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Start gRPC server
	listener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		setupLog.Error(err, "unable to create gRPC listener")
		os.Exit(1)
	}

	go func() {
		setupLog.Info("Starting gRPC server", "addr", *grpcAddr)
		if err := grpcServer.Serve(listener); err != nil {
			setupLog.Error(err, "gRPC server failed")
			os.Exit(1)
		}
	}()

	// Start metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		server := &http.Server{
			Addr:    *metricsAddr,
			Handler: mux,
		}

		setupLog.Info("Starting metrics server", "addr", *metricsAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "metrics server failed")
		}
	}()

	// Wait for shutdown signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	setupLog.Info("Shutting down gracefully...")

	// Graceful shutdown
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	done := make(chan bool, 1)
	go func() {
		grpcServer.GracefulStop()
		done <- true
	}()

	select {
	case <-done:
		setupLog.Info("gRPC server stopped gracefully")
	case <-time.After(30 * time.Second):
		setupLog.Info("Force stopping gRPC server")
		grpcServer.Stop()
	}
}

func createTLSGRPCServer(tlsDir string) (*grpc.Server, error) {
	// Load server certificate and key
	certFile := fmt.Sprintf("%s/tls.crt", tlsDir)
	keyFile := fmt.Sprintf("%s/tls.key", tlsDir)
	caFile := fmt.Sprintf("%s/ca.crt", tlsDir)

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificates: %w", err)
	}

	// Load CA certificate if present (for mTLS)
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	if _, err := os.Stat(caFile); err == nil {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}

		config.ClientCAs = caCertPool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}

	creds := credentials.NewTLS(config)
	return grpc.NewServer(grpc.Creds(creds)), nil
}
