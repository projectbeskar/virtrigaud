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

	// Use authentication callback to provide credentials
	auth := &libvirt.ConnectAuth{
		CredTypes: []libvirt.ConnectCredentialType{
			libvirt.CRED_AUTHNAME,
			libvirt.CRED_USERNAME, 
			libvirt.CRED_PASSPHRASE,
		},
		Callback: p.authCallback,
	}
	
	log.Printf("DEBUG: Connecting with auth callback, URI: %s", authenticatedURI)
	conn, err := libvirt.NewConnectWithAuth(authenticatedURI, auth, 0)
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

// authCallback handles authentication requests from libvirt
func (p *Provider) authCallback(creds []libvirt.ConnectCredential) int {
	log.Printf("DEBUG: Auth callback called with %d credentials", len(creds))
	
	for i := range creds {
		log.Printf("DEBUG: Processing credential type %v, prompt: '%s'", creds[i].Type, creds[i].Prompt)
		
		switch creds[i].Type {
		case libvirt.CRED_USERNAME, libvirt.CRED_AUTHNAME:
			if p.credentials.Username != "" {
				creds[i].Result = p.credentials.Username
				log.Printf("DEBUG: Provided username '%s' for prompt '%s'", p.credentials.Username, creds[i].Prompt)
			} else {
				log.Printf("DEBUG: No username available for prompt '%s'", creds[i].Prompt)
				return -1
			}
		case libvirt.CRED_PASSPHRASE:
			if p.credentials.Password != "" {
				creds[i].Result = p.credentials.Password
				log.Printf("DEBUG: Provided password (len=%d) for prompt '%s'", len(p.credentials.Password), creds[i].Prompt)
			} else {
				log.Printf("DEBUG: No password available for prompt '%s'", creds[i].Prompt)
				return -1
			}
		default:
			log.Printf("DEBUG: Unsupported credential type %v for prompt '%s'", creds[i].Type, creds[i].Prompt)
			return -1
		}
	}
	
	log.Printf("DEBUG: Auth callback completed successfully")
	return 0
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
