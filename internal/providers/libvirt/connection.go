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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	libvirt "libvirt.org/go/libvirt"
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

	// Check for TLS certificates
	if certData, ok := secret.Data["tls.crt"]; ok {
		creds.CertData = string(certData)
	}
	if keyData, ok := secret.Data["tls.key"]; ok {
		creds.KeyData = string(keyData)
	}
	if caData, ok := secret.Data["ca.crt"]; ok {
		creds.CAData = string(caData)
	}

	p.credentials = creds
	return nil
}

// connect establishes a connection to Libvirt
func (p *Provider) connect(ctx context.Context) error {
	// Parse the endpoint URL
	uri := p.config.Spec.Endpoint

	// For local connections, use default URI if not specified
	if uri == "" {
		uri = "qemu:///system"
	}

	// Establish connection
	// For MVP, we'll use simple connection without complex auth
	// TODO: Implement proper authentication for remote connections
	conn, err := libvirt.NewConnect(uri)
	if err != nil {
		return fmt.Errorf("failed to connect to Libvirt: %w", err)
	}

	p.conn = conn
	return nil
}

// TODO: Implement authentication for remote connections
// needsAuth determines if authentication is needed based on URI
// buildAuth creates authentication configuration
// These will be implemented in a future version

// disconnect closes the Libvirt connection

// ensureConnection ensures we have a valid connection to Libvirt
func (p *Provider) ensureConnection(ctx context.Context) error {
	if p.conn == nil {
		return p.connect(ctx)
	}

	// Test the connection
	alive, err := p.conn.IsAlive()
	if err != nil || !alive {
		return p.connect(ctx)
	}

	return nil
}
