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

package vsphere

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"


)

// loadCredentials loads credentials from the referenced Kubernetes secret
func (p *Provider) loadCredentials(ctx context.Context) error {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      p.config.Spec.CredentialSecretRef.Name,
		Namespace: p.config.Namespace,
	}

	// Use the specified namespace if provided
	if p.config.Spec.CredentialSecretRef.Namespace != "" {
		secretKey.Namespace = p.config.Spec.CredentialSecretRef.Namespace
	}

	if err := p.k8sClient.Get(ctx, secretKey, secret); err != nil {
		return fmt.Errorf("failed to get credentials secret: %w", err)
	}

	creds := &Credentials{}

	// Check for username/password authentication
	if username, ok := secret.Data["username"]; ok {
		creds.Username = string(username)
	}
	if password, ok := secret.Data["password"]; ok {
		creds.Password = string(password)
	}

	// Check for token authentication
	if token, ok := secret.Data["token"]; ok {
		creds.Token = string(token)
	}

	// Validate that we have some form of authentication
	if creds.Username == "" && creds.Token == "" {
		return fmt.Errorf("credentials secret must contain either 'username' or 'token'")
	}

	if creds.Username != "" && creds.Password == "" {
		return fmt.Errorf("credentials secret with 'username' must also contain 'password'")
	}

	p.credentials = creds
	return nil
}

// connect establishes a connection to vSphere
func (p *Provider) connect(ctx context.Context) error {
	// Parse the endpoint URL
	u, err := url.Parse(p.config.Spec.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid vSphere endpoint URL: %w", err)
	}

	// Set up authentication
	if p.credentials.Username != "" {
		u.User = url.UserPassword(p.credentials.Username, p.credentials.Password)
	}

	// Create SOAP client
	soapClient := soap.NewClient(u, p.config.Spec.InsecureSkipVerify)
	
	// Configure TLS if needed
	if !p.config.Spec.InsecureSkipVerify {
		soapClient.DefaultTransport().TLSClientConfig = &tls.Config{
			ServerName: u.Hostname(),
		}
	}

	// Create vSphere client
	vimClient, err := vim25.NewClient(ctx, soapClient)
	if err != nil {
		return fmt.Errorf("failed to create vSphere VIM client: %w", err)
	}

	p.client = &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	// Login to vSphere
	if err := p.login(ctx); err != nil {
		return fmt.Errorf("failed to login to vSphere: %w", err)
	}

	// Create finder for object discovery
	p.finder = find.NewFinder(p.client.Client, true)

	// Set default datacenter if we can find one
	if err := p.setupDefaultDatacenter(ctx); err != nil {
		// Log warning but don't fail - user might specify datacenter in placement
		// TODO: Add proper logging
	}

	return nil
}

// login authenticates with vSphere
func (p *Provider) login(ctx context.Context) error {
	// The govmomi client will use the credentials from the URL
	// No additional parameters needed for Login method
	return p.client.Login(ctx, nil)
}

// setupDefaultDatacenter finds and sets a default datacenter for the finder
func (p *Provider) setupDefaultDatacenter(ctx context.Context) error {
	datacenters, err := p.finder.DatacenterList(ctx, "*")
	if err != nil {
		return err
	}

	if len(datacenters) > 0 {
		// Use the first datacenter as default
		p.finder.SetDatacenter(datacenters[0])
		return nil
	}

	return fmt.Errorf("no datacenters found")
}

// disconnect closes the vSphere connection
func (p *Provider) disconnect(ctx context.Context) error {
	if p.client != nil {
		return p.client.Logout(ctx)
	}
	return nil
}

// ensureConnection ensures we have a valid connection to vSphere
func (p *Provider) ensureConnection(ctx context.Context) error {
	if p.client == nil {
		return p.connect(ctx)
	}

	// Test the connection
	if !p.client.Valid() {
		return p.connect(ctx)
	}

	return nil
}
