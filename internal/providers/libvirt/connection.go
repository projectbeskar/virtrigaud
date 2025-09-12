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

	// Strategy 2: Test virsh command-line approach
	if p.credentials.Username != "" && p.credentials.Password != "" {
		log.Printf("DEBUG: Testing virsh command-line approach")
		
		// Build URI with authentication
		u, err := url.Parse(uri)
		if err != nil {
			return nil, fmt.Errorf("failed to parse URI: %w", err)
		}
		
		// Add username if not present
		if strings.Contains(u.Scheme, "ssh") && p.credentials.Username != "" {
			if u.User == nil {
				u.User = url.User(p.credentials.Username)
				log.Printf("DEBUG: Added username to URI: %s", p.credentials.Username)
			}
		}
		
		// Add SSH options for containers
		query := u.Query()
		query.Set("no_verify", "1")
		query.Set("no_tty", "1")
		u.RawQuery = query.Encode()
		
		virshURI := u.String()
		
		// Test virsh connection
		if err := p.testVirshConnection(ctx, virshURI); err != nil {
			log.Printf("DEBUG: virsh approach failed: %v", err)
			log.Printf("DEBUG: Falling back to libvirt-go password authentication")
			
			// Fall back to original password approach
			conn, err := p.connectWithPassword(ctx, uri)
			if err != nil {
				return nil, fmt.Errorf("both virsh and libvirt-go failed - virsh: %v, libvirt-go: %w", err, err)
			}
			return conn, nil
		}
		
		log.Printf("DEBUG: virsh approach successful! Consider switching to virsh-based provider")
		// For now, still try libvirt-go to maintain compatibility
		conn, err := p.connectWithPassword(ctx, uri)
		if err != nil {
			return nil, fmt.Errorf("virsh works but libvirt-go failed: %w", err)
		}
		return conn, nil
	}

	return nil, fmt.Errorf("no valid authentication method available (tried SSH key, virsh, and password)")
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

// testVirshConnection tests if virsh command-line approach works
func (p *Provider) testVirshConnection(ctx context.Context, uri string) error {
	log.Printf("DEBUG: Testing virsh connection approach")
	
	// Build environment with credentials
	env := os.Environ()
	env = append(env, fmt.Sprintf("LIBVIRT_DEFAULT_URI=%s", uri))
	
	// Test basic virsh connectivity
	cmd := exec.Command("virsh", "list", "--all")
	cmd.Env = env
	
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	
	if err := cmd.Run(); err != nil {
		log.Printf("DEBUG: virsh failed: %v, stderr: %s", err, stderr.String())
		return fmt.Errorf("virsh connection failed: %w", err)
	}
	
	log.Printf("DEBUG: virsh connection successful!")
	log.Printf("DEBUG: virsh output: %s", stdout.String())
	return nil
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
			libvirt.CRED_ECHOPROMPT,   // For interactive prompts
			libvirt.CRED_NOECHOPROMPT, // For password prompts
		},
		Callback: func(creds []*libvirt.ConnectCredential) {
			for i := range creds {
				switch creds[i].Type {
				case libvirt.CRED_AUTHNAME:
					creds[i].Result = p.credentials.Username
					log.Printf("DEBUG: Provided username for authentication: %s", p.credentials.Username)
				case libvirt.CRED_PASSPHRASE, libvirt.CRED_NOECHOPROMPT:
					creds[i].Result = p.credentials.Password
					log.Printf("DEBUG: Provided password for authentication (type: %d)", creds[i].Type)
				case libvirt.CRED_ECHOPROMPT:
					creds[i].Result = p.credentials.Username // Usually username for echo prompts
					log.Printf("DEBUG: Provided response to echo prompt: %s", p.credentials.Username)
				default:
					log.Printf("DEBUG: Unknown credential type requested: %d", creds[i].Type)
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
		// Write SSH key to a temporary file and use keyfile parameter
		keyfilePath, err := p.writeSSHKeyToTempFile()
		if err != nil {
			return "", fmt.Errorf("failed to write SSH key to temp file: %w", err)
		}

		// Add keyfile parameter to URI
		query := u.Query()
		query.Set("keyfile", keyfilePath)
		query.Set("no_verify", "1")
		query.Set("no_tty", "1")
		u.RawQuery = query.Encode()

		log.Printf("DEBUG: SSH key written to temp file: %s", keyfilePath)
	} else {
		log.Printf("DEBUG: No SSH private key available, trying default SSH config")

		// Add SSH options for password authentication
		query := u.Query()
		query.Set("no_verify", "1")
		query.Set("no_tty", "1")
		u.RawQuery = query.Encode()
	}

	finalURI := u.String()
	log.Printf("DEBUG: Final SSH key URI: %s", finalURI)
	return finalURI, nil
}

// writeSSHKeyToTempFile writes the SSH private key to a temporary file
func (p *Provider) writeSSHKeyToTempFile() (string, error) {
	log.Printf("DEBUG: Writing SSH key to temporary file")

	// Try different writable locations in order of preference
	tempDirs := []string{
		"/etc/virtrigaud/credentials", // Kubernetes secret mount (should be writable for our key)
		"/home/app/.ssh",              // User home SSH directory
		"/tmp",                        // Standard temp directory
		"/var/tmp",                    // Alternative temp directory
	}

	var tempFile *os.File
	var err error
	var tempDir string

	// Try each directory until we find one that works
	for _, dir := range tempDirs {
		tempDir = dir
		// Ensure directory exists and is writable
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Printf("DEBUG: Cannot create directory %s: %v", dir, err)
			continue
		}

		// Try to create temporary file
		tempFile, err = os.CreateTemp(dir, "libvirt_ssh_key_*.pem")
		if err != nil {
			log.Printf("DEBUG: Cannot create temp file in %s: %v", dir, err)
			continue
		}
		break
	}

	if tempFile == nil {
		return "", fmt.Errorf("failed to create temporary file in any directory: %v", err)
	}

	defer tempFile.Close()

	// Write the SSH private key
	if _, err := tempFile.WriteString(p.credentials.SSHPrivateKey); err != nil {
		os.Remove(tempFile.Name())
		return "", fmt.Errorf("failed to write SSH key to temp file: %w", err)
	}

	// Set proper permissions (readable only by owner)
	if err := os.Chmod(tempFile.Name(), 0600); err != nil {
		os.Remove(tempFile.Name())
		return "", fmt.Errorf("failed to set permissions on SSH key file: %w", err)
	}

	log.Printf("DEBUG: SSH key written to: %s (in %s)", tempFile.Name(), tempDir)
	return tempFile.Name(), nil
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
