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
	"time"
)

// VirshProvider implements a virsh command-line based libvirt provider
type VirshProvider struct {
	config      *ProviderConfig
	credentials *Credentials
	uri         string
	env         []string
}

// VirshDomain represents a VM domain from virsh list output
type VirshDomain struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// VirshError represents an error from virsh command execution
type VirshError struct {
	Command  string
	ExitCode int
	Stderr   string
	Stdout   string
}

func (e *VirshError) Error() string {
	return fmt.Sprintf("virsh command '%s' failed (exit code %d): stderr=%s, stdout=%s",
		e.Command, e.ExitCode, e.Stderr, e.Stdout)
}

// NewVirshProvider creates a new virsh-based provider
func NewVirshProvider(config *ProviderConfig) *VirshProvider {
	return &VirshProvider{
		config: config,
	}
}

// Initialize sets up the virsh provider with credentials and connection
func (v *VirshProvider) Initialize(ctx context.Context) error {
	log.Printf("INFO Initializing virsh-based libvirt provider")

	// Load credentials from environment variables (secure approach)
	if err := v.loadCredentialsFromEnv(); err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}

	// Build libvirt URI and environment
	if err := v.setupConnection(); err != nil {
		return fmt.Errorf("failed to setup connection: %w", err)
	}

	// Test the connection
	if err := v.testConnection(ctx); err != nil {
		return fmt.Errorf("failed to connect to libvirt: %w", err)
	}

	log.Printf("INFO Successfully initialized virsh provider with endpoint: %s", v.uri)
	return nil
}

// loadCredentialsFromEnv loads credentials from environment variables for security
func (v *VirshProvider) loadCredentialsFromEnv() error {
	log.Printf("INFO Loading credentials from environment variables (secure method)")

	v.credentials = &Credentials{}

	// Load username from environment
	if username := os.Getenv("LIBVIRT_USERNAME"); username != "" {
		v.credentials.Username = username
		log.Printf("INFO Successfully loaded username from env username_length=%d", len(v.credentials.Username))
	}

	// Load password from environment
	if password := os.Getenv("LIBVIRT_PASSWORD"); password != "" {
		v.credentials.Password = password
		log.Printf("INFO Successfully loaded password from env password_length=%d", len(v.credentials.Password))
	}

	// Load SSH private key from environment
	if sshKey := os.Getenv("LIBVIRT_SSH_PRIVATE_KEY"); sshKey != "" {
		v.credentials.SSHPrivateKey = sshKey
		log.Printf("INFO Successfully loaded SSH private key from env ssh_key_length=%d", len(v.credentials.SSHPrivateKey))
	}

	// Fallback: Load from mounted files if environment variables not set
	if v.credentials.Username == "" {
		if usernameData, err := os.ReadFile("/etc/virtrigaud/credentials/username"); err == nil {
			v.credentials.Username = strings.TrimSpace(string(usernameData))
			log.Printf("INFO Fallback: loaded username from file username_length=%d", len(v.credentials.Username))
		}
	}

	if v.credentials.Password == "" {
		if passwordData, err := os.ReadFile("/etc/virtrigaud/credentials/password"); err == nil {
			v.credentials.Password = strings.TrimSpace(string(passwordData))
			log.Printf("INFO Fallback: loaded password from file password_length=%d", len(v.credentials.Password))
		}
	}

	if v.credentials.SSHPrivateKey == "" {
		if sshKeyData, err := os.ReadFile("/etc/virtrigaud/credentials/ssh-privatekey"); err == nil {
			v.credentials.SSHPrivateKey = strings.TrimSpace(string(sshKeyData))
			log.Printf("INFO Fallback: loaded SSH private key from file ssh_key_length=%d", len(v.credentials.SSHPrivateKey))
		}
	}

	if v.credentials.Username == "" && v.credentials.Password == "" && v.credentials.SSHPrivateKey == "" {
		return fmt.Errorf("no valid credentials found in environment variables or mounted files")
	}

	return nil
}

// setupConnection prepares the libvirt URI and environment for virsh commands
func (v *VirshProvider) setupConnection() error {
	// Get base URI from config
	uri := v.config.Spec.Endpoint
	if uri == "" {
		uri = "qemu:///system" // Default local connection
	}

	// Parse and enhance URI for authentication
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("failed to parse URI: %w", err)
	}

	// Add username to SSH URIs
	if strings.Contains(parsedURI.Scheme, "ssh") && v.credentials.Username != "" {
		if parsedURI.User == nil {
			parsedURI.User = url.User(v.credentials.Username)
			log.Printf("INFO Added username to libvirt URI: %s", v.credentials.Username)
		}
	}

	// Add SSH options for container environments
	if strings.Contains(parsedURI.Scheme, "ssh") {
		query := parsedURI.Query()
		query.Set("no_verify", "1") // Skip host key verification
		query.Set("no_tty", "1")    // Non-interactive mode
		parsedURI.RawQuery = query.Encode()
		log.Printf("INFO Added SSH options for container environment")
	}

	v.uri = parsedURI.String()

	// Set up environment variables for virsh
	v.env = os.Environ()
	v.env = append(v.env, fmt.Sprintf("LIBVIRT_DEFAULT_URI=%s", v.uri))

	// Set SSH authentication via environment variables for non-interactive use
	if v.credentials.Password != "" {
		// Use sshpass for non-interactive password authentication
		v.env = append(v.env, fmt.Sprintf("SSHPASS=%s", v.credentials.Password))
		
		// Set SSH options for non-interactive authentication
		v.env = append(v.env, "SSH_ASKPASS_REQUIRE=never")
		
		// Accept host keys automatically (for containers)
		v.env = append(v.env, "SSH_OPTIONS=-o StrictHostKeyChecking=accept-new -o PasswordAuthentication=yes")
		
		log.Printf("INFO Configured non-interactive SSH authentication via sshpass")
	}

	log.Printf("INFO Configured virsh environment with URI: %s", v.uri)
	return nil
}

// testConnection verifies that virsh can connect to the libvirt hypervisor
func (v *VirshProvider) testConnection(ctx context.Context) error {
	log.Printf("INFO Testing virsh connection to libvirt")

	// Run basic virsh command to test connectivity
	result, err := v.runVirshCommand(ctx, "version")
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	log.Printf("INFO Connection successful! Libvirt version: %s", strings.TrimSpace(result.Stdout))

	// Test domain listing to verify full functionality
	domains, err := v.listDomains(ctx)
	if err != nil {
		return fmt.Errorf("connection established but domain listing failed: %w", err)
	}

	log.Printf("INFO Successfully listed %d domains", len(domains))
	return nil
}

// VirshResult represents the result of a virsh command execution
type VirshResult struct {
	Command  string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// runVirshCommand executes a virsh command with proper environment and error handling
func (v *VirshProvider) runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	start := time.Now()
	
	var cmd *exec.Cmd
	var command string
	
	// Use sshpass for non-interactive SSH authentication if password is available
	if v.credentials.Password != "" && strings.Contains(v.uri, "ssh://") {
		// Build sshpass command with SSH options
		sshpassArgs := []string{
			"-e", // Read password from SSHPASS environment variable
			"virsh",
		}
		sshpassArgs = append(sshpassArgs, args...)
		
		cmd = exec.CommandContext(ctx, "sshpass", sshpassArgs...)
		command = "sshpass -e virsh " + strings.Join(args, " ")
		
		// Set environment with SSH options
		cmd.Env = v.env
		cmd.Env = append(cmd.Env, "SSH_OPTIONS=-o StrictHostKeyChecking=accept-new -o PasswordAuthentication=yes -o PubkeyAuthentication=no")
	} else {
		// Standard virsh command for local or key-based connections
		cmd = exec.CommandContext(ctx, "virsh", args...)
		command = "virsh " + strings.Join(args, " ")
		cmd.Env = v.env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Printf("DEBUG Executing: %s", command)

	// Run the command
	err := cmd.Run()
	duration := time.Since(start)

	result := &VirshResult{
		Command:  command,
		ExitCode: cmd.ProcessState.ExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		log.Printf("ERROR Command failed: %s (exit code: %d, duration: %v)",
			command, result.ExitCode, duration)
		log.Printf("ERROR Stderr: %s", result.Stderr)
		return result, &VirshError{
			Command:  command,
			ExitCode: result.ExitCode,
			Stderr:   result.Stderr,
			Stdout:   result.Stdout,
		}
	}

	log.Printf("DEBUG Command successful: %s (duration: %v)", command, duration)
	return result, nil
}

// listDomains lists all domains (VMs) using virsh
func (v *VirshProvider) listDomains(ctx context.Context) ([]VirshDomain, error) {
	// Get all domains (running and shut off)
	result, err := v.runVirshCommand(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}

	var domains []VirshDomain
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Get domain state
		stateResult, err := v.runVirshCommand(ctx, "domstate", line)
		state := "unknown"
		if err == nil {
			state = strings.TrimSpace(stateResult.Stdout)
		}

		domains = append(domains, VirshDomain{
			ID:    fmt.Sprintf("%d", i),
			Name:  line,
			State: state,
		})
	}

	return domains, nil
}

// createDomain creates a new domain from XML definition
func (v *VirshProvider) createDomain(ctx context.Context, xmlDef string) error {
	log.Printf("INFO Creating domain from XML definition")

	// Create temporary file for XML definition
	tmpFile, err := os.CreateTemp("", "domain_*.xml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write XML definition
	if _, err := tmpFile.WriteString(xmlDef); err != nil {
		return fmt.Errorf("failed to write XML definition: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Create domain using virsh
	_, err = v.runVirshCommand(ctx, "create", tmpFile.Name())
	if err != nil {
		return fmt.Errorf("failed to create domain: %w", err)
	}

	log.Printf("INFO Successfully created domain")
	return nil
}

// defineDomain defines a domain from XML (creates but doesn't start)
func (v *VirshProvider) defineDomain(ctx context.Context, xmlDef string) error {
	log.Printf("INFO Defining domain from XML definition")

	// Create temporary file for XML definition
	tmpFile, err := os.CreateTemp("", "domain_*.xml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write XML definition
	if _, err := tmpFile.WriteString(xmlDef); err != nil {
		return fmt.Errorf("failed to write XML definition: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Define domain using virsh
	_, err = v.runVirshCommand(ctx, "define", tmpFile.Name())
	if err != nil {
		return fmt.Errorf("failed to define domain: %w", err)
	}

	log.Printf("INFO Successfully defined domain")
	return nil
}

// startDomain starts a defined domain
func (v *VirshProvider) startDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Starting domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "start", domainName)
	if err != nil {
		return fmt.Errorf("failed to start domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully started domain: %s", domainName)
	return nil
}

// stopDomain stops a running domain
func (v *VirshProvider) stopDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Stopping domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "shutdown", domainName)
	if err != nil {
		return fmt.Errorf("failed to stop domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully stopped domain: %s", domainName)
	return nil
}

// destroyDomain forcefully stops a domain
func (v *VirshProvider) destroyDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Force stopping domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "destroy", domainName)
	if err != nil {
		return fmt.Errorf("failed to destroy domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully destroyed domain: %s", domainName)
	return nil
}

// undefineDomain removes a domain definition
func (v *VirshProvider) undefineDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Undefining domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "undefine", domainName)
	if err != nil {
		return fmt.Errorf("failed to undefine domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully undefined domain: %s", domainName)
	return nil
}

// getDomainXML retrieves the XML definition of a domain
func (v *VirshProvider) getDomainXML(ctx context.Context, domainName string) (string, error) {
	result, err := v.runVirshCommand(ctx, "dumpxml", domainName)
	if err != nil {
		return "", fmt.Errorf("failed to get domain XML for %s: %w", domainName, err)
	}

	return result.Stdout, nil
}

// getDomainInfo gets detailed information about a domain
func (v *VirshProvider) getDomainInfo(ctx context.Context, domainName string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "dominfo", domainName)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain info for %s: %w", domainName, err)
	}

	info := make(map[string]string)
	lines := strings.Split(result.Stdout, "\n")

	for _, line := range lines {
		if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			info[key] = value
		}
	}

	return info, nil
}

// Cleanup performs any necessary cleanup operations
func (v *VirshProvider) Cleanup() error {
	log.Printf("INFO Cleaning up virsh provider")

	// No persistent connections to close with virsh approach
	// All commands are stateless

	return nil
}
