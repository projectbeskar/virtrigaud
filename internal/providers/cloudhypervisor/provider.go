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

package cloudhypervisor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ProviderType is the Provider type this package serves.
const ProviderType = "cloudhypervisor"

// listHostsHelloTimeout bounds the Hello ListHosts makes to a host that has
// none on its current connection.
const listHostsHelloTimeout = 5 * time.Second

// Options configures New.
type Options struct {
	// ClientCertificate is the Provider's identity certificate and key,
	// presented to every agent. Required.
	ClientCertificate tls.Certificate
	// ProviderUID, when set, is the UID of the Provider resource: an agent
	// whose certificate records an enrolment with another Provider is
	// refused.
	ProviderUID string
	// APIVersion is the agent.v1 API version the Provider speaks; zero uses
	// APIVersion. Only tests set it, to stand for another release.
	APIVersion uint32
	// Breaker configures each host's circuit breaker; nil uses
	// DefaultBreakerThreshold and DefaultBreakerReset.
	Breaker *resilience.Config
	// DialContext, when set, opens the connections to agents.
	DialContext func(ctx context.Context, addr string) (net.Conn, error)
	// Now is the clock agent certificates are judged at; nil uses time.Now.
	Now func() time.Time
	// Logger receives one line per refused host, Hello and inventory
	// change; never request bytes. Nil uses slog.Default().
	Logger *slog.Logger
}

// Provider is the clustered Provider that routes to per-host agents. It is
// safe for concurrent use.
type Provider struct {
	providerv1.UnimplementedProviderServer
	opts    Options
	log     *slog.Logger
	pool    *pool
	watcher *inventoryfile.Watcher
}

var _ providerv1.ProviderServer = (*Provider)(nil)

// New returns a Provider that fronts the hosts of inv. Nothing is dialed
// until a call needs a host.
func New(opts Options, inv hostsecret.Inventory) (*Provider, error) {
	if len(opts.ClientCertificate.Certificate) == 0 || opts.ClientCertificate.PrivateKey == nil {
		return nil, errors.New("cloudhypervisor: the Provider's client certificate and key are required")
	}
	if opts.APIVersion == 0 {
		opts.APIVersion = APIVersion
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	p := &Provider{opts: opts, log: opts.Logger}
	p.pool = newPool(&p.opts, p.log)
	if err := p.pool.reconcile(inv); err != nil {
		return nil, fmt.Errorf("cloudhypervisor: %w", err)
	}
	return p, nil
}

// Reconcile makes the fronted hosts those of inv (inventoryfile.Reconciler).
// The connection of a host that left inv, or whose endpoint or TLS material
// changed, is closed before Reconcile returns.
func (p *Provider) Reconcile(inv hostsecret.Inventory) error { return p.pool.reconcile(inv) }

// Close stops following the inventory file, if the Provider does, and
// closes every connection to an agent.
func (p *Provider) Close() error {
	var err error
	if p.watcher != nil {
		err = p.watcher.Close()
	}
	p.pool.close()
	return err
}

// Register registers the Provider's gRPC service on s, with the forwarded
// methods reading and sending their requests as raw bytes (ServiceDesc).
func (p *Provider) Register(s grpc.ServiceRegistrar) { s.RegisterService(ServiceDesc(), p) }

// ServiceDesc is the provider.v1 Provider service description in which every
// forwarded method reads its request as the exact bytes that arrived and
// relays them unchanged. The Provider must be registered with it, not with
// providerv1.RegisterProviderServer: the generated description decodes and
// re-encodes the request, which keeps its meaning but not necessarily its
// bytes.
func ServiceDesc() *grpc.ServiceDesc {
	d := providerv1.Provider_ServiceDesc
	d.Methods = make([]grpc.MethodDesc, len(providerv1.Provider_ServiceDesc.Methods))
	copy(d.Methods, providerv1.Provider_ServiceDesc.Methods)
	for i, m := range d.Methods {
		full := "/" + d.ServiceName + "/" + m.MethodName
		if _, ok := routes[full]; ok {
			d.Methods[i].Handler = rawHandler(full)
		}
	}
	return &d
}

// rawHandler is the handler of a forwarded method: the request is read into
// a raw frame and handed to Provider.handle, through the server's
// interceptors.
func rawHandler(method string) grpc.MethodHandler {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := &emptypb.Empty{}
		if err := dec(in); err != nil {
			return nil, err
		}
		p, ok := srv.(*Provider)
		if !ok {
			return nil, status.Error(codes.Internal, "the service is not a cloudhypervisor Provider")
		}
		run := func(ctx context.Context, req any) (any, error) {
			frame, ok := req.(*emptypb.Empty)
			if !ok {
				return nil, status.Error(codes.Internal, "an interceptor replaced the request")
			}
			return p.handle(ctx, method, frameBytes(frame))
		}
		if interceptor == nil {
			return run(ctx, in)
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: method}, run)
	}
}

// typed serves a forwarded method called with a decoded request (the
// generated service description): the request is encoded again and handled
// as raw bytes, and the answer decoded into resp.
func typed[T proto.Message](ctx context.Context, p *Provider, method string, req proto.Message, resp T) (T, error) {
	var zero T
	b, err := proto.Marshal(req)
	if err != nil {
		return zero, status.Error(codes.InvalidArgument, "the request does not encode")
	}
	out, err := p.handle(ctx, method, b)
	if err != nil {
		return zero, err
	}
	if frame, ok := out.(*emptypb.Empty); ok {
		if err := proto.Unmarshal(frameBytes(frame), resp); err != nil {
			return zero, status.Error(codes.Internal, "the host's answer does not decode")
		}
		return resp, nil
	}
	got, ok := out.(T)
	if !ok {
		return zero, status.Error(codes.Internal, "unexpected answer type")
	}
	return got, nil
}

// Create implements providerv1.ProviderServer.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	return typed(ctx, p, providerv1.Provider_Create_FullMethodName, req, &providerv1.CreateResponse{})
}

// Delete implements providerv1.ProviderServer.
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	return typed(ctx, p, providerv1.Provider_Delete_FullMethodName, req, &providerv1.TaskResponse{})
}

// Power implements providerv1.ProviderServer.
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	return typed(ctx, p, providerv1.Provider_Power_FullMethodName, req, &providerv1.TaskResponse{})
}

// Reconfigure implements providerv1.ProviderServer.
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	return typed(ctx, p, providerv1.Provider_Reconfigure_FullMethodName, req, &providerv1.TaskResponse{})
}

// Describe implements providerv1.ProviderServer.
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	return typed(ctx, p, providerv1.Provider_Describe_FullMethodName, req, &providerv1.DescribeResponse{})
}

// TaskStatus implements providerv1.ProviderServer.
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	return typed(ctx, p, providerv1.Provider_TaskStatus_FullMethodName, req, &providerv1.TaskStatusResponse{})
}

// GetHostInfo implements providerv1.ProviderServer.
func (p *Provider) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	return typed(ctx, p, providerv1.Provider_GetHostInfo_FullMethodName, req, &providerv1.HostInfo{})
}

// Validate implements providerv1.ProviderServer: the Provider itself is
// healthy while it runs; a host's health is reported per host (ListHosts).
func (p *Provider) Validate(context.Context, *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	return &providerv1.ValidateResponse{Ok: true, Message: fmt.Sprintf("fronting %d hosts", len(p.pool.ids()))}, nil
}

// ListHosts implements providerv1.ProviderServer: every fronted host, from
// the inventory, READY when its agent passed its Hello on the current
// connection and its breaker is not open. A host with no Hello on its
// current connection gets one first, bounded by listHostsHelloTimeout.
func (p *Provider) ListHosts(ctx context.Context, _ *providerv1.ListHostsRequest) (*providerv1.ListHostsResponse, error) {
	resp := &providerv1.ListHostsResponse{}
	for _, id := range p.pool.ids() {
		e, ok := p.pool.get(id)
		if !ok {
			continue
		}
		if made, _ := e.currentHello(); !made {
			hctx, cancel := context.WithTimeout(ctx, listHostsHelloTimeout)
			_ = p.pool.helloed(hctx, e)
			cancel()
		}
		health := providerv1.HostHealth_HOST_HEALTH_NOT_READY
		if e.ready() {
			health = providerv1.HostHealth_HOST_HEALTH_READY
		}
		e.mu.Lock()
		labels := make(map[string]string, len(e.spec.Labels))
		for k, v := range e.spec.Labels {
			labels[k] = v
		}
		e.mu.Unlock()
		resp.Hosts = append(resp.Hosts, &providerv1.HostInfo{Id: id, Health: health, Labels: labels})
	}
	return resp, nil
}
