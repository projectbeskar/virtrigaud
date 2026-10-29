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

// Command fakeprovider is Spike 1B's test-only clustered provider
// (docs/performance/spike-1b-control-plane-baseline.md). It is NOT a
// supported provider and is built only by hack/bench/spike1b.
//
// It serves the real provider gRPC contract through the real provider SDK
// server (TLS/auth resolution, health, middleware) and simulates a cluster
// of hosts, each run by a per-host agent with bounded execution concurrency
// and configurable latency. Nothing executes: a "VM" is a record. It models
// the control-plane conversation with an agent-based provider, not host
// execution, networking, storage or hypervisor behaviour.
//
// The Provider controller deploys it like any provider (spec.runtime.image)
// and mounts the host-inventory Secret; the fake serves the hosts listed
// there. An admin HTTP endpoint (FAKE_ADMIN_ADDR, default :8090) serves
// GET /executions (the execution state, for the invariant auditor), GET and
// POST /config (latency and fault injection at run time) and /metrics.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// fakeMetrics are the fake provider's own Prometheus metrics.
type fakeMetrics struct {
	rpcTotal          *prometheus.CounterVec
	rpcDuration       *prometheus.HistogramVec
	slotWait          *prometheus.HistogramVec
	faults            *prometheus.CounterVec
	createRepeat      prometheus.Counter
	duplicateIdentity prometheus.Counter
	vms               prometheus.Gauge
	hosts             prometheus.Gauge
}

// newFakeMetrics registers the metrics in reg.
func newFakeMetrics(reg prometheus.Registerer) *fakeMetrics {
	buckets := prometheus.ExponentialBuckets(0.0005, 2, 20) // 0.5 ms .. ~262 s
	m := &fakeMetrics{
		rpcTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "spike_fake_rpc_total", Help: "RPCs served, by method and gRPC code.",
		}, []string{"method", "code"}),
		rpcDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "spike_fake_rpc_duration_seconds", Help: "Server-side RPC duration, by method.", Buckets: buckets,
		}, []string{"method"}),
		slotWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "spike_fake_host_slot_wait_seconds", Help: "Wait for a host agent execution slot.", Buckets: buckets,
		}, []string{"method"}),
		faults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "spike_fake_injected_faults_total", Help: "Injected faults.",
		}, []string{"kind"}),
		createRepeat: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "spike_fake_create_repeat_total", Help: "Creates that bound an existing VM of the same owner.",
		}),
		duplicateIdentity: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "spike_fake_duplicate_identity_total", Help: "Times one owner UID ran on two hosts at once (must stay 0).",
		}),
		vms:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "spike_fake_vms", Help: "VM records held."}),
		hosts: prometheus.NewGauge(prometheus.GaugeOpts{Name: "spike_fake_hosts", Help: "Hosts in the inventory."}),
	}
	reg.MustRegister(m.rpcTotal, m.rpcDuration, m.slotWait, m.faults, m.createRepeat, m.duplicateIdentity, m.vms, m.hosts)
	return m
}

func main() {
	var port, healthPort int
	flag.IntVar(&port, "port", 9443, "gRPC server port")
	flag.IntVar(&healthPort, "health-port", 8080, "Health check port")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := configFromEnv()
	if err != nil {
		logger.Error("bad configuration", "error", err)
		os.Exit(1)
	}
	var current atomic.Pointer[config]
	current.Store(cfg)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := newFakeMetrics(reg)
	st := newState(current.Load, m, logger)
	spec, err := workload.ParseSpec(os.Getenv(envSeed))
	if err != nil {
		logger.Error("bad seed", "error", err)
		os.Exit(1)
	}
	st.seed(spec)
	logger.Info("seeded", "spec", spec.String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.watchInventory(ctx)
	go serveAdmin(ctx, logger, st, &current, reg)

	tlsResolution, tlsErr := server.ResolveTLSAndAuth()
	if tlsErr != nil && !errors.Is(tlsErr, server.ErrInsecureModeOptedIn) {
		logger.Error("Failed to resolve TLS configuration", "error", tlsErr)
		os.Exit(1)
	}
	sc := server.DefaultConfig()
	sc.Port = port
	sc.HealthPort = healthPort
	sc.Logger = logger
	sc.TLS = tlsResolution.TLS
	sc.Middleware = &middleware.Config{
		Recovery: &middleware.RecoveryConfig{Enabled: true, Logger: logger},
		Auth:     tlsResolution.Auth,
	}
	sc.MetricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	srv, err := server.New(sc)
	if err != nil {
		logger.Error("Failed to create server", "error", err)
		os.Exit(1)
	}
	srv.RegisterProvider(&fakeServer{st: st, m: m})
	logger.Info("Starting Spike 1B fake clustered provider (test-only)", "port", port)
	if err := srv.Serve(ctx); err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

// serveAdmin serves the admin endpoint until ctx ends.
func serveAdmin(ctx context.Context, logger *slog.Logger, st *state, current *atomic.Pointer[config], reg *prometheus.Registry) {
	addr := os.Getenv(envAdminAddr)
	if addr == "" {
		addr = defaultAdminAddr
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/executions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st.snapshot())
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			next := *current.Load()
			next.Latency = maps.Clone(next.Latency)
			next.UnreachableHosts = slices.Clone(next.UnreachableHosts)
			if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			current.Store(&next)
			logger.Info("configuration changed", "config", next)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current.Load())
	})
	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("admin endpoint failed", "error", err)
	}
}
