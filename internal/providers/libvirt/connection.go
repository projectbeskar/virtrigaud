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
	"bytes"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
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

// connectWithAuth establishes an authenticated connection to Libvirt with fallback strategy
func (p *Provider) connectWithAuth(ctx context.Context, uri string) (*libvirt.Connect, error) {
	if p.credentials == nil {
		return nil, fmt.Errorf("credentials not loaded for authenticated connection")
	}

	// Strategy 1: Try SSH key authentication first (if available)
	if p.credentials.SSHPrivateKey != "" {
		log.Printf("DEBUG: Attempting SSH key authentication")
		conn, err := p.connectWithSSHKey(ctx, uri)
		if err != nil {
			log.Printf("DEBUG: SSH key authentication failed: %v", err)
			log.Printf("DEBUG: Falling back to password authentication")
		} else {
			log.Printf("DEBUG: Successfully established SSH key authenticated connection")
			return conn, nil
		}
	}

	// Strategy 2: Fall back to password authentication
	if p.credentials.Username != "" && p.credentials.Password != "" {
		log.Printf("DEBUG: Attempting password authentication")
		conn, err := p.connectWithPassword(ctx, uri)
		if err != nil {
			return nil, fmt.Errorf("failed to connect with password: %w", err)
		}
		log.Printf("DEBUG: Successfully established password authenticated connection")
		return conn, nil
	}

	return nil, fmt.Errorf("no valid authentication method available (tried SSH key and password)")
}

// connectWithSSHKey attempts SSH key authentication
func (p *Provider) connectWithSSHKey(ctx context.Context, uri string) (*libvirt.Connect, error) {
	// Build URI with SSH key authentication
	authenticatedURI, err := p.buildSSHKeyURI(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to build SSH key URI: %w", err)
	}

	log.Printf("DEBUG: Connecting to libvirt with SSH key, URI: %s", authenticatedURI)

	// Simple connection - libvirt handles SSH authentication via SSH agent
	conn, err := libvirt.NewConnect(authenticatedURI)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with SSH key: %w", err)
	}

	return conn, nil
}

// connectWithPassword attempts password authentication via libvirt callback
func (p *Provider) connectWithPassword(ctx context.Context, uri string) (*libvirt.Connect, error) {
	// Parse and modify URI to include username
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URI: %w", err)
	}

	// Add username to URI if provided and not already present
	if strings.Contains(u.Scheme, "ssh") && p.credentials.Username != "" {
		if u.User == nil {
			u.User = url.User(p.credentials.Username)
			log.Printf("DEBUG: Added username to URI: %s", p.credentials.Username)
		}
	}

	// Add SSH options to disable host key verification (common in containers)
	query := u.Query()
	query.Set("no_verify", "1")
	query.Set("no_tty", "1")
	u.RawQuery = query.Encode()

	authenticatedURI := u.String()
	log.Printf("DEBUG: Connecting to libvirt with password, URI: %s", authenticatedURI)

	// Use libvirt authentication callback for password
	auth := &libvirt.ConnectAuth{
		CredType: []libvirt.ConnectCredentialType{
			libvirt.CRED_AUTHNAME,
			libvirt.CRED_PASSPHRASE,
		},
		Callback: func(creds []*libvirt.ConnectCredential) {
			for i := range creds {
				switch creds[i].Type {
				case libvirt.CRED_AUTHNAME:
					creds[i].Result = p.credentials.Username
					log.Printf("DEBUG: Provided username for authentication")
				case libvirt.CRED_PASSPHRASE:
					creds[i].Result = p.credentials.Password
					log.Printf("DEBUG: Provided password for authentication")
				}
			}
		},
	}

	conn, err := libvirt.NewConnectWithAuth(authenticatedURI, auth, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to connect with password: %w", err)
	}

	return conn, nil
}

// buildSSHKeyURI builds a URI with SSH key authentication
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
		// Use SSH agent instead of writing key files
		err := p.setupSSHAgent()
		if err != nil {
			return "", fmt.Errorf("failed to setup SSH agent: %w", err)
		}

		log.Printf("DEBUG: SSH agent configured with private key")
	} else {
		log.Printf("DEBUG: No SSH private key available, trying default SSH agent/config")
	}

	// Add SSH options to disable host key verification (common in containers)
	query := u.Query()
	query.Set("no_verify", "1")
	query.Set("no_tty", "1")
	u.RawQuery = query.Encode()

	finalURI := u.String()
	log.Printf("DEBUG: Final SSH key URI: %s", finalURI)
	return finalURI, nil
}

// setupSSHAgent configures SSH agent with the private key
func (p *Provider) setupSSHAgent() error {
	log.Printf("DEBUG: Setting up SSH agent with private key")

	// Start SSH agent if not already running
	if err := p.startSSHAgent(); err != nil {
		return fmt.Errorf("failed to start SSH agent: %w", err)
	}

	// Add the key to SSH agent using stdin (no file needed!)
	if err := p.addKeyToSSHAgentFromStdin(); err != nil {
		return fmt.Errorf("failed to add key to SSH agent: %w", err)
	}

	log.Printf("DEBUG: SSH agent setup completed successfully")
	return nil
}

// startSSHAgent starts the SSH agent if not already running
func (p *Provider) startSSHAgent() error {
	// Check if SSH_AUTH_SOCK is already set (agent running)
	if authSock := os.Getenv("SSH_AUTH_SOCK"); authSock != "" {
		log.Printf("DEBUG: SSH agent already running at: %s", authSock)
		return nil
	}

	// Start ssh-agent and capture output
	cmd := exec.Command("ssh-agent", "-s")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to start ssh-agent: %w", err)
	}

	// Parse the output to set environment variables
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "SSH_AUTH_SOCK") {
			parts := strings.Split(line, "=")
			if len(parts) >= 2 {
				value := strings.TrimSuffix(parts[1], ";")
				os.Setenv("SSH_AUTH_SOCK", value)
				log.Printf("DEBUG: Set SSH_AUTH_SOCK=%s", value)
			}
		} else if strings.Contains(line, "SSH_AGENT_PID") {
			parts := strings.Split(line, "=")
			if len(parts) >= 2 {
				value := strings.TrimSuffix(parts[1], ";")
				os.Setenv("SSH_AGENT_PID", value)
				log.Printf("DEBUG: Set SSH_AGENT_PID=%s", value)
			}
		}
	}

	return nil
}

// addKeyToSSHAgentFromStdin adds the SSH private key to the running SSH agent via stdin
func (p *Provider) addKeyToSSHAgentFromStdin() error {
	log.Printf("DEBUG: Adding key to SSH agent via stdin")

	// Use ssh-add with stdin to add the key (no file needed!)
	cmd := exec.Command("ssh-add", "-")

	// Provide the private key via stdin
	cmd.Stdin = strings.NewReader(p.credentials.SSHPrivateKey)

	// Capture both stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh-add failed: %w, stderr: %s", err, stderr.String())
	}

	log.Printf("DEBUG: Successfully added key to SSH agent via stdin")
	if stdout.Len() > 0 {
		log.Printf("DEBUG: ssh-add output: %s", stdout.String())
	}

	return nil
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
