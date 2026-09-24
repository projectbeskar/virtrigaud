/*
Copyright 2026.

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
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// TestGenerateDefaultCloudInit_ProvisionsNoCredentials pins the security
// property of the default user-data applied when a VirtualMachine supplies
// none: it must never create a login user, authorize an SSH key, set a
// password, or grant sudo. A previous version shipped a fixed ed25519 key with
// NOPASSWD sudo, giving the key holder root on every such VM.
func TestGenerateDefaultCloudInit_ProvisionsNoCredentials(t *testing.T) {
	p := &Provider{}
	out := p.generateDefaultCloudInit("web-01")

	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Fatalf("default cloud-init must start with #cloud-config, got:\n%s", out)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("default cloud-init is not valid YAML: %v\n%s", err, out)
	}

	// Top-level keys that grant or configure guest access must be absent.
	for _, key := range []string{
		"users", "user", "ssh_authorized_keys", "password", "chpasswd",
		"ssh_pwauth", "disable_root", "write_files", "bootcmd",
	} {
		if _, ok := doc[key]; ok {
			t.Errorf("default cloud-init must not set %q", key)
		}
	}

	// Belt and braces against a credential hidden anywhere in the document.
	for _, needle := range []string{"ssh-ed25519", "ssh-rsa", "ecdsa-sha2", "NOPASSWD", "sudo"} {
		if strings.Contains(out, needle) {
			t.Errorf("default cloud-init must not contain %q:\n%s", needle, out)
		}
	}

	hostname, ok := doc["hostname"].(string)
	if !ok || hostname != "web-01" {
		t.Errorf("hostname = %v, want %q", doc["hostname"], "web-01")
	}

	// qemu-guest-agent is the one thing the default must keep: IP discovery
	// and in-guest operations depend on it.
	packages, ok := doc["packages"].([]interface{})
	if !ok {
		t.Fatalf("packages missing or wrong type: %T", doc["packages"])
	}
	if len(packages) != 1 || packages[0] != "qemu-guest-agent" {
		t.Errorf("packages = %v, want [qemu-guest-agent]", packages)
	}
	if !strings.Contains(out, "systemctl start qemu-guest-agent") {
		t.Errorf("default cloud-init must start qemu-guest-agent:\n%s", out)
	}
}
