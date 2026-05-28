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

package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
)

// Mount-path constants pinned by the manager-side wiring (PR-1 of ADR-0003).
// They MUST match the values used in
// internal/controller/provider_controller.go so that whatever the manager
// mounts is what the provider reads.
const (
	// ProviderTLSMountPath is the in-pod directory where the manager mounts
	// the provider's TLS material (server cert + key + CA bundle).
	ProviderTLSMountPath = "/etc/virtrigaud/tls"

	// ProviderTLSCertFile is the absolute path to the server certificate.
	ProviderTLSCertFile = ProviderTLSMountPath + "/tls.crt"

	// ProviderTLSKeyFile is the absolute path to the server private key.
	ProviderTLSKeyFile = ProviderTLSMountPath + "/tls.key"

	// ProviderTLSCAFile is the absolute path to the CA bundle used to
	// verify the *manager's* client certificate.
	ProviderTLSCAFile = ProviderTLSMountPath + "/ca.crt"
)

// Environment-variable names consumed by ResolveTLSAndAuth.
const (
	// EnvAllowedSANs is a comma-separated list of SAN/CN values the
	// provider will accept from the manager's client certificate. Empty
	// (or unset) means "trust any cert signed by the configured CA"
	// (ADR-0003 decision #5).
	EnvAllowedSANs = "VIRTRIGAUD_PROVIDER_ALLOWED_SANS"

	// EnvInsecure is the explicit escape hatch. When set to "true" AND
	// the TLS material is absent on disk, the provider starts in
	// plaintext mode with a loud WARN log. Any other combination (cert
	// files present, or env-var unset) keeps TLS-mandatory semantics.
	EnvInsecure = "VIRTRIGAUD_PROVIDER_INSECURE"
)

// ErrInsecureModeOptedIn is returned by ResolveTLSAndAuth when the operator
// has explicitly opted out of TLS via VIRTRIGAUD_PROVIDER_INSECURE=true AND
// no TLS material is present on disk. It is a sentinel, not a hard error —
// the provider main is expected to log a WARN and continue with nil TLS.
var ErrInsecureModeOptedIn = errors.New("provider started in plaintext mode (VIRTRIGAUD_PROVIDER_INSECURE=true)")

// TLSResolution is the outcome of ResolveTLSAndAuth. Exactly one of TLS or
// Insecure is meaningful:
//
//   - TLS != nil → provider should bind with this TLSConfig and Auth
//   - Insecure == true → provider should bind plaintext and log a WARN
//
// Auth is always populated; in the insecure branch its RequireTLS field is
// false so the middleware does no auth checks.
type TLSResolution struct {
	TLS      *TLSConfig
	Auth     *middleware.AuthConfig
	Insecure bool
}

// ResolveTLSAndAuth inspects the on-disk TLS material at the canonical
// ProviderTLSMountPath and translates the environment-variable contract
// from ADR-0003 PR-2 into a TLSConfig + AuthConfig pair.
//
// Behavior table:
//
//	| files present | EnvInsecure | result                                                       |
//	|---------------|-------------|--------------------------------------------------------------|
//	| yes           | (any)       | TLS+Auth populated; RequireClientCert+RequireTLS; AutoReload  |
//	| no            | "true"      | nil TLS; Insecure=true; ErrInsecureModeOptedIn               |
//	| no            | unset/other | hard error naming the missing files                          |
//
// On the TLS-present branch AutoReload is set so the SDK server
// hot-reloads the leaf cert/key on file change (ADR-0003 PR-3). The CA
// bundle does not hot-reload; rotating it requires a provider restart.
//
// The hard-error branch is deliberate: a v0.3.7 provider that finds no
// certs on disk and no explicit opt-out must refuse to start, otherwise
// we silently regress to plaintext on a misconfigured upgrade.
//
// AllowedSANs is read from EnvAllowedSANs, comma-separated, whitespace
// trimmed, empty entries dropped. An empty/unset env-var yields an empty
// allow-list, which the SDK middleware treats as permissive (any cert
// from the CA accepted) per ADR-0003 decision #5.
func ResolveTLSAndAuth() (*TLSResolution, error) {
	certPath := ProviderTLSCertFile
	keyPath := ProviderTLSKeyFile
	caPath := ProviderTLSCAFile

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	caExists := fileExists(caPath)

	allFilesPresent := certExists && keyExists && caExists
	allFilesAbsent := !certExists && !keyExists && !caExists

	allowedSANs := parseAllowedSANs(os.Getenv(EnvAllowedSANs))

	switch {
	case allFilesPresent:
		return &TLSResolution{
			TLS: &TLSConfig{
				CertFile:          certPath,
				KeyFile:           keyPath,
				CAFile:            caPath,
				RequireClientCert: true,
				// Hot-reload the leaf cert by default on the TLS path —
				// operators provisioning certs expect rotation to work
				// without a pod restart (ADR-0003 PR-3). The CA bundle
				// still requires a restart to rotate (see
				// TLSConfig.AutoReload).
				AutoReload: true,
			},
			Auth: &middleware.AuthConfig{
				RequireTLS:  true,
				AllowedSANs: allowedSANs,
			},
		}, nil

	case allFilesAbsent && isInsecureOptedIn():
		return &TLSResolution{
			TLS:      nil,
			Auth:     &middleware.AuthConfig{RequireTLS: false},
			Insecure: true,
		}, ErrInsecureModeOptedIn

	case allFilesAbsent:
		return nil, fmt.Errorf(
			"TLS material missing at %s and %s is not set to \"true\"; "+
				"either provision %s/{tls.crt,tls.key,ca.crt} or set "+
				"%s=true to opt into plaintext (audit-flagged)",
			ProviderTLSMountPath, EnvInsecure,
			ProviderTLSMountPath, EnvInsecure,
		)

	default:
		// Partial: some present, some missing. Always a hard error.
		var missing []string
		if !certExists {
			missing = append(missing, certPath)
		}
		if !keyExists {
			missing = append(missing, keyPath)
		}
		if !caExists {
			missing = append(missing, caPath)
		}
		return nil, fmt.Errorf(
			"TLS material partially present; missing required files: %s",
			strings.Join(missing, ", "),
		)
	}
}

// parseAllowedSANs splits the comma-separated env-var value into a
// trimmed, non-empty list. An empty/unset value yields a nil slice.
func parseAllowedSANs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// isInsecureOptedIn returns true when the operator has explicitly set
// VIRTRIGAUD_PROVIDER_INSECURE=true. Anything else (unset, "false",
// "1", "yes", etc.) keeps TLS-mandatory semantics — we want the value
// the operator types to be the actual word "true", not a fuzzy match.
func isInsecureOptedIn() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(EnvInsecure)), "true")
}

// fileExists returns true when the path resolves to a regular file. Any
// other outcome (missing, directory, symlink-to-missing) returns false.
func fileExists(path string) bool {
	// Resolve symlinks via os.Stat so a Kubernetes-projected Secret
	// (which uses ..data symlinks) is read correctly.
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
