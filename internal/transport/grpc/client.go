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

package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/internal/rpc/provider/v1"
)

// Client wraps a gRPC provider client and implements the contracts.Provider interface
type Client struct {
	conn   *grpc.ClientConn
	client providerv1.ProviderClient
}

// NewClient creates a new gRPC provider client
func NewClient(ctx context.Context, endpoint string, tlsConfig *TLSConfig) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var opts []grpc.DialOption

	if tlsConfig != nil {
		creds, err := buildTLSCredentials(tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS credentials: %w", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Add retry and timeout configurations
	opts = append(opts,
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
		),
	)

	conn, err := grpc.DialContext(ctx, endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to provider at %s: %w", endpoint, err)
	}

	client := providerv1.NewProviderClient(conn)
	return &Client{
		conn:   conn,
		client: client,
	}, nil
}

// Close closes the gRPC connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// Validate implements contracts.Provider
func (c *Client) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.client.Validate(ctx, &providerv1.ValidateRequest{})
	if err != nil {
		return c.mapGRPCError("validate", err)
	}

	if !resp.Ok {
		return fmt.Errorf("provider validation failed: %s", resp.Message)
	}

	return nil
}

// Create implements contracts.Provider
func (c *Client) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	grpcReq, err := c.convertCreateRequest(req)
	if err != nil {
		return contracts.CreateResponse{}, fmt.Errorf("failed to convert create request: %w", err)
	}

	resp, err := c.client.Create(ctx, grpcReq)
	if err != nil {
		return contracts.CreateResponse{}, c.mapGRPCError("create", err)
	}

	result := contracts.CreateResponse{
		ID: resp.Id,
	}

	if resp.Task != nil {
		result.TaskRef = resp.Task.Id
	}

	return result, nil
}

// Delete implements contracts.Provider
func (c *Client) Delete(ctx context.Context, id string) (taskRef string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	resp, err := c.client.Delete(ctx, &providerv1.DeleteRequest{Id: id})
	if err != nil {
		return "", c.mapGRPCError("delete", err)
	}

	if resp.Task != nil {
		return resp.Task.Id, nil
	}

	return "", nil
}

// Power implements contracts.Provider
func (c *Client) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	grpcOp, err := c.convertPowerOp(op)
	if err != nil {
		return "", fmt.Errorf("invalid power operation: %w", err)
	}

	resp, err := c.client.Power(ctx, &providerv1.PowerRequest{
		Id: id,
		Op: grpcOp,
	})
	if err != nil {
		return "", c.mapGRPCError("power", err)
	}

	if resp.Task != nil {
		return resp.Task.Id, nil
	}

	return "", nil
}

// Reconfigure implements contracts.Provider
func (c *Client) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return "", fmt.Errorf("failed to marshal desired configuration: %w", err)
	}

	resp, err := c.client.Reconfigure(ctx, &providerv1.ReconfigureRequest{
		Id:          id,
		DesiredJson: string(desiredJSON),
	})
	if err != nil {
		return "", c.mapGRPCError("reconfigure", err)
	}

	if resp.Task != nil {
		return resp.Task.Id, nil
	}

	return "", nil
}

// Describe implements contracts.Provider
func (c *Client) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.client.Describe(ctx, &providerv1.DescribeRequest{Id: id})
	if err != nil {
		return contracts.DescribeResponse{}, c.mapGRPCError("describe", err)
	}

	// Parse provider raw data
	var providerRaw map[string]string
	if resp.ProviderRawJson != "" {
		// First unmarshal to map[string]any, then convert to map[string]string
		var rawData map[string]any
		if err := json.Unmarshal([]byte(resp.ProviderRawJson), &rawData); err != nil {
			// Log error but don't fail the entire operation
			providerRaw = map[string]string{"parseError": err.Error()}
		} else {
			// Convert map[string]any to map[string]string
			providerRaw = make(map[string]string)
			for k, v := range rawData {
				providerRaw[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	return contracts.DescribeResponse{
		Exists:      resp.Exists,
		PowerState:  resp.PowerState,
		IPs:         resp.Ips,
		ConsoleURL:  resp.ConsoleUrl,
		ProviderRaw: providerRaw,
	}, nil
}

// IsTaskComplete implements contracts.Provider
func (c *Client) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := c.client.TaskStatus(ctx, &providerv1.TaskStatusRequest{
		Task: &providerv1.TaskRef{Id: taskRef},
	})
	if err != nil {
		return false, c.mapGRPCError("taskStatus", err)
	}

	if resp.Error != "" {
		return true, fmt.Errorf("task failed: %s", resp.Error)
	}

	return resp.Done, nil
}

// convertCreateRequest converts contracts.CreateRequest to gRPC format
func (c *Client) convertCreateRequest(req contracts.CreateRequest) (*providerv1.CreateRequest, error) {
	grpcReq := &providerv1.CreateRequest{
		Name: req.Name,
		Tags: req.Tags,
	}

	// Convert UserData
	if req.UserData != nil {
		grpcReq.UserData = []byte(req.UserData.CloudInitData)
	}

	// Convert each component to JSON
	if classData, err := json.Marshal(req.Class); err == nil {
		grpcReq.ClassJson = string(classData)
	}

	if imageData, err := json.Marshal(req.Image); err == nil {
		grpcReq.ImageJson = string(imageData)
	}

	if networksData, err := json.Marshal(req.Networks); err == nil {
		grpcReq.NetworksJson = string(networksData)
	}

	if disksData, err := json.Marshal(req.Disks); err == nil {
		grpcReq.DisksJson = string(disksData)
	}

	if req.Placement != nil {
		if placementData, err := json.Marshal(req.Placement); err == nil {
			grpcReq.PlacementJson = string(placementData)
		}
	}

	return grpcReq, nil
}

// convertPowerOp converts contracts.PowerOp to gRPC format
func (c *Client) convertPowerOp(op contracts.PowerOp) (providerv1.PowerOp, error) {
	switch op {
	case contracts.PowerOpOn:
		return providerv1.PowerOp_POWER_OP_ON, nil
	case contracts.PowerOpOff:
		return providerv1.PowerOp_POWER_OP_OFF, nil
	case contracts.PowerOpReboot:
		return providerv1.PowerOp_POWER_OP_REBOOT, nil
	default:
		return providerv1.PowerOp_POWER_OP_UNSPECIFIED, fmt.Errorf("unsupported power operation: %s", op)
	}
}

// mapGRPCError converts gRPC errors to contracts errors where possible
func (c *Client) mapGRPCError(operation string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s failed: %w", operation, err)
	}

	switch st.Code() {
	case codes.NotFound:
		return contracts.NewNotFoundError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	case codes.InvalidArgument:
		return contracts.NewInvalidSpecError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	case codes.Unavailable, codes.DeadlineExceeded:
		return contracts.NewRetryableError(fmt.Sprintf("%s: %s", operation, st.Message()), err)
	default:
		return fmt.Errorf("%s failed: %s", operation, st.Message())
	}
}

// TLSConfig represents TLS configuration for gRPC clients
type TLSConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
	Insecure bool
}

// buildTLSCredentials builds gRPC transport credentials from TLS config
func buildTLSCredentials(config *TLSConfig) (credentials.TransportCredentials, error) {
	if config.Insecure {
		return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}), nil
	}

	tlsConfig := &tls.Config{}

	// Load client certificate if provided
	if config.CertFile != "" && config.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided
	if config.CAFile != "" {
		caCert, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return credentials.NewTLS(tlsConfig), nil
}
