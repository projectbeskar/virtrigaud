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

package tracing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	otrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// Service names
	ServiceManager         = "virtrigaud-manager"
	ServiceProviderLibvirt = "virtrigaud-provider-libvirt"
	ServiceProviderVSphere = "virtrigaud-provider-vsphere"
)

// Config holds tracing configuration
type Config struct {
	Enabled           bool
	Endpoint          string
	ServiceName       string
	ServiceVersion    string
	SamplingRatio     float64
	InsecureTransport bool
}

// DefaultConfig returns default tracing configuration
func DefaultConfig(serviceName, version string) *Config {
	return &Config{
		Enabled:           getEnvBool("VIRTRIGAUD_TRACING_ENABLED", false),
		Endpoint:          getEnv("VIRTRIGAUD_TRACING_ENDPOINT", ""),
		ServiceName:       serviceName,
		ServiceVersion:    version,
		SamplingRatio:     getEnvFloat("VIRTRIGAUD_TRACING_SAMPLING_RATIO", 0.1),
		InsecureTransport: getEnvBool("VIRTRIGAUD_TRACING_INSECURE", true),
	}
}

// Setup initializes OpenTelemetry tracing
func Setup(ctx context.Context, config *Config) (func(), error) {
	if !config.Enabled {
		// Set up a no-op otracer provider
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func() {}, nil
	}

	if config.Endpoint == "" {
		return nil, fmt.Errorf("tracing endpoint is required when tracing is enabled")
	}

	// Create OTLP exporter
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(config.Endpoint),
	}

	if config.InsecureTransport {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(config.ServiceName),
			semconv.ServiceVersion(config.ServiceVersion),
			attribute.String("service.namespace", "virtrigaud"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create otracer provider
	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(res),
		trace.WithSampler(trace.TraceIDRatioBased(config.SamplingRatio)),
	)

	// Set global otracer provider
	otel.SetTracerProvider(tp)

	// Set global propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Return shutdown function
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			// Log error but don't fail shutdown
			fmt.Printf("Error shutting down otracer provider: %v\n", err)
		}
	}, nil
}

// GetTracer returns a otracer for the given name
func GetTracer(name string) otrace.Tracer {
	return otel.Tracer(name)
}

// StartSpan starts a new span with the given name and options
func StartSpan(ctx context.Context, name string, opts ...otrace.SpanStartOption) (context.Context, otrace.Span) {
	otracer := otel.Tracer("virtrigaud")
	return otracer.Start(ctx, name, opts...)
}

// AddEvent adds an event to the current span
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := otrace.SpanFromContext(ctx)
	span.AddEvent(name, otrace.WithAttributes(attrs...))
}

// SetAttributes sets attributes on the current span
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := otrace.SpanFromContext(ctx)
	span.SetAttributes(attrs...)
}

// RecordError records an error on the current span
func RecordError(ctx context.Context, err error) {
	span := otrace.SpanFromContext(ctx)
	span.RecordError(err)
}

// Common attribute keys for virtrigaud
var (
	// VM attributes
	AttrVMNamespace = attribute.Key("vm.namespace")
	AttrVMName      = attribute.Key("vm.name")
	AttrVMID        = attribute.Key("vm.id")

	// Provider attributes
	AttrProviderType      = attribute.Key("provider.type")
	AttrProviderNamespace = attribute.Key("provider.namespace")
	AttrProviderName      = attribute.Key("provider.name")
	AttrProviderEndpoint  = attribute.Key("provider.endpoint")

	// Operation attributes
	AttrOperation = attribute.Key("operation")
	AttrTaskRef   = attribute.Key("task.ref")
	AttrOutcome   = attribute.Key("outcome")

	// RPC attributes
	AttrRPCMethod = attribute.Key("rpc.method")
	AttrRPCCode   = attribute.Key("rpc.code")

	// Resource attributes
	AttrResourceKind      = attribute.Key("resource.kind")
	AttrResourceNamespace = attribute.Key("resource.namespace")
	AttrResourceName      = attribute.Key("resource.name")
)

// Span names for common operations
const (
	SpanVMReconcile = "vm.reconcile"
	SpanVMCreate    = "vm.create"
	SpanVMDelete    = "vm.delete"
	SpanVMPower     = "vm.power"
	SpanVMDescribe  = "vm.describe"

	SpanProviderValidate    = "provider.validate"
	SpanProviderCreate      = "provider.create"
	SpanProviderDelete      = "provider.delete"
	SpanProviderPower       = "provider.power"
	SpanProviderDescribe    = "provider.describe"
	SpanProviderReconfigure = "provider.reconfigure"
	SpanProviderTaskStatus  = "provider.task_status"

	SpanCircuitBreaker = "circuit_breaker.check"
	SpanIPDiscovery    = "ip.discovery"
)

// Helper functions for common span patterns

// StartVMSpan starts a span for a VM operation
func StartVMSpan(ctx context.Context, operation, namespace, name string) (context.Context, otrace.Span) {
	return StartSpan(ctx, fmt.Sprintf("vm.%s", operation),
		otrace.WithAttributes(
			AttrVMNamespace.String(namespace),
			AttrVMName.String(name),
			AttrOperation.String(operation),
		),
	)
}

// StartProviderSpan starts a span for a provider operation
func StartProviderSpan(ctx context.Context, operation, providerType string) (context.Context, otrace.Span) {
	return StartSpan(ctx, fmt.Sprintf("provider.%s", operation),
		otrace.WithAttributes(
			AttrProviderType.String(providerType),
			AttrOperation.String(operation),
		),
	)
}

// StartRPCSpan starts a span for an RPC call
func StartRPCSpan(ctx context.Context, method, providerType string) (context.Context, otrace.Span) {
	return StartSpan(ctx, fmt.Sprintf("rpc.%s", method),
		otrace.WithAttributes(
			AttrRPCMethod.String(method),
			AttrProviderType.String(providerType),
		),
	)
}

// GRPCClientInterceptor returns a gRPC client interceptor that adds tracing
func GRPCClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx, span := StartSpan(ctx, fmt.Sprintf("grpc.client.%s", method),
			otrace.WithSpanKind(otrace.SpanKindClient),
			otrace.WithAttributes(
				AttrRPCMethod.String(method),
			),
		)
		defer span.End()

		// Inject tracing context into gRPC metadata
		ctx = InjectGRPCContext(ctx)

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			span.RecordError(err)
		}

		return err
	}
}

// GRPCServerInterceptor returns a gRPC server interceptor that adds tracing
func GRPCServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Extract tracing context from gRPC metadata
		ctx = ExtractGRPCContext(ctx)

		ctx, span := StartSpan(ctx, fmt.Sprintf("grpc.server.%s", info.FullMethod),
			otrace.WithSpanKind(otrace.SpanKindServer),
			otrace.WithAttributes(
				AttrRPCMethod.String(info.FullMethod),
			),
		)
		defer span.End()

		resp, err := handler(ctx, req)
		if err != nil {
			span.RecordError(err)
		}

		return resp, err
	}
}

// InjectGRPCContext injects tracing context into gRPC metadata
func InjectGRPCContext(ctx context.Context) context.Context {
	// This would typically use otel's gRPC instrumentation
	// For now, we'll return the context as-is
	return ctx
}

// ExtractGRPCContext extracts tracing context from gRPC metadata
func ExtractGRPCContext(ctx context.Context) context.Context {
	// This would typically use otel's gRPC instrumentation
	// For now, we'll return the context as-is
	return ctx
}

// Helper functions for environment variable parsing
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}
