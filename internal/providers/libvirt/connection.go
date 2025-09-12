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

// connectWithAuth establishes an authenticated connection to Libvirt
func (p *Provider) connectWithAuth(ctx context.Context, uri string) (*libvirt.Connect, error) {
	if p.credentials == nil {
		return nil, fmt.Errorf("credentials not loaded for authenticated connection")
	}

	// For SSH connections, we need to set up the URI with embedded credentials
	// This is a temporary approach while we work on proper callback authentication
	authenticatedURI, err := p.buildAuthenticatedURI(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to build authenticated URI: %w", err)
	}

	// For SSH connections with username/password, use environment variables
	// as a workaround for libvirt authentication issues
	if strings.Contains(authenticatedURI, "ssh://") {
		log.Printf("DEBUG: Setting SSH environment variables for authentication")
		os.Setenv("LIBVIRT_AUTH_FILE", "/tmp/libvirt_auth")
		
		// Create a temporary auth file
		authContent := fmt.Sprintf(`[credentials-ssh]
authname=%s
username=%s
password=%s
`, p.credentials.Username, p.credentials.Username, p.credentials.Password)
		
		if err := os.WriteFile("/tmp/libvirt_auth", []byte(authContent), 0600); err != nil {
			log.Printf("DEBUG: Failed to create auth file: %v", err)
		} else {
			log.Printf("DEBUG: Created auth file for SSH authentication")
		}
	}
	
	log.Printf("DEBUG: Connecting with libvirt, URI: %s", authenticatedURI)
	conn, err := libvirt.NewConnect(authenticatedURI)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with authentication: %w", err)
	}

	return conn, nil
}

// buildAuthenticatedURI builds a URI with embedded authentication for SSH connections
func (p *Provider) buildAuthenticatedURI(uri string) (string, error) {
	log.Printf("DEBUG: Building authenticated URI from: %s", uri)
	log.Printf("DEBUG: Credentials available: username='%s', password_len=%d", 
		p.credentials.Username, len(p.credentials.Password))
	
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("failed to parse URI: %w", err)
	}

	// For SSH connections, embed username in the URI
	if strings.Contains(parsedURI.Scheme, "ssh") && p.credentials.Username != "" {
		// If the URI doesn't already have a user, add it
		if parsedURI.User == nil {
			parsedURI.User = url.User(p.credentials.Username)
			log.Printf("DEBUG: Added username to URI: %s", p.credentials.Username)
		} else {
			log.Printf("DEBUG: URI already has user: %s", parsedURI.User.Username())
		}
	}

	finalURI := parsedURI.String()
	log.Printf("DEBUG: Final authenticated URI: %s", finalURI)
	return finalURI, nil
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
