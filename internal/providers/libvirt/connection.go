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
	"net/url"
	"os"
	"strings"

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

	// Check for SSH keys
	if sshPrivateKey, ok := secret.Data["ssh-privatekey"]; ok {
		creds.SSHPrivateKey = string(sshPrivateKey)
	}
	if sshPublicKey, ok := secret.Data["ssh-publickey"]; ok {
		creds.SSHPublicKey = string(sshPublicKey)
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

	// Check if this is a remote connection that needs authentication
	if p.needsAuth(uri) {
		conn, err := p.connectWithAuth(ctx, uri)
		if err != nil {
			return fmt.Errorf("failed to connect to Libvirt with authentication: %w", err)
		}
		p.conn = conn
		return nil
	}

	// Establish simple connection for local/unauthenticated connections
	conn, err := libvirt.NewConnect(uri)
	if err != nil {
		return fmt.Errorf("failed to connect to Libvirt: %w", err)
	}

	p.conn = conn
	return nil
}

// needsAuth determines if authentication is needed based on URI
func (p *Provider) needsAuth(uri string) bool {
	// Parse the URI to determine transport
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return false
	}

	// Check for remote transports that require authentication
	return strings.Contains(parsedURI.Scheme, "ssh") ||
		strings.Contains(parsedURI.Scheme, "tls") ||
		(parsedURI.Host != "" && parsedURI.Host != "localhost")
}

// connectWithAuth establishes an authenticated connection to Libvirt using SSH key
func (p *Provider) connectWithAuth(ctx context.Context, uri string) (*libvirt.Connect, error) {
	if p.credentials == nil {
		return nil, fmt.Errorf("credentials not loaded for authenticated connection")
	}

	// Build URI with SSH key authentication
	authenticatedURI, err := p.buildSSHKeyURI(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to build SSH key URI: %w", err)
	}

	log.Printf("DEBUG: Connecting to libvirt with SSH key, URI: %s", authenticatedURI)

	// Simple connection - libvirt handles SSH authentication via keyfile parameter
	conn, err := libvirt.NewConnect(authenticatedURI)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with SSH key: %w", err)
	}

	log.Printf("DEBUG: Successfully established SSH key authenticated libvirt connection")
	return conn, nil
}

// buildSSHKeyURI builds a URI with SSH key authentication like your example
func (p *Provider) buildSSHKeyURI(uri string) (string, error) {
	log.Printf("DEBUG: Building SSH key URI from: %s", uri)

	// Parse the base URI
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("failed to parse URI: %w", err)
	}

	// Add username to URI if provided and not already present
	if strings.Contains(u.Scheme, "ssh") && p.credentials.Username != "" {
		if u.User == nil {
			u.User = url.User(p.credentials.Username)
			log.Printf("DEBUG: Added username to URI: %s", p.credentials.Username)
		}
	}

	// Check if we have SSH private key
	if p.credentials.SSHPrivateKey != "" {
		// Write SSH private key to a temporary file
		keyPath, err := p.writeSSHKeyToFile()
		if err != nil {
			return "", fmt.Errorf("failed to write SSH key to file: %w", err)
		}

		// Add keyfile parameter to URI
		q := u.Query()
		q.Set("keyfile", keyPath)
		u.RawQuery = q.Encode()

		log.Printf("DEBUG: Added keyfile parameter: %s", keyPath)
	} else {
		log.Printf("DEBUG: No SSH private key available, trying default SSH agent")
	}

	finalURI := u.String()
	log.Printf("DEBUG: Final SSH key URI: %s", finalURI)
	return finalURI, nil
}

// writeSSHKeyToFile writes the SSH private key to a temporary file
func (p *Provider) writeSSHKeyToFile() (string, error) {
	// Create SSH key file in writable location
	keyPath := "/home/app/.ssh/id_rsa"

	// Ensure .ssh directory exists
	if err := os.MkdirAll("/home/app/.ssh", 0700); err != nil {
		return "", fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Write the private key
	if err := os.WriteFile(keyPath, []byte(p.credentials.SSHPrivateKey), 0600); err != nil {
		return "", fmt.Errorf("failed to write SSH private key: %w", err)
	}

	log.Printf("DEBUG: SSH private key written to: %s", keyPath)
	return keyPath, nil
}

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
