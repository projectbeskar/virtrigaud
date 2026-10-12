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

package contracts

import "strings"

// maxKubernetesNameLen is the longest name SanitizeVMName returns (a DNS
// label).
const maxKubernetesNameLen = 63

// SanitizeVMName turns a hypervisor VM name into the name of the
// VirtualMachine that adopts it: lower-cased, '_', '.' and ' ' turned into
// '-', every other character outside [a-z0-9-] dropped, leading and trailing
// '-' trimmed, and cut to 63 characters. It may return "" (a name with no
// usable character). It is the single owner of the rule: the adoption
// controller names VirtualMachines with it, and a provider that matches an
// unstamped VM's name against its owner (Proxmox's owner-checked Delete)
// applies the same rule.
func SanitizeVMName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, " ", "-")
	var result strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		}
	}
	name = strings.Trim(result.String(), "-")
	if len(name) > maxKubernetesNameLen {
		name = strings.Trim(name[:maxKubernetesNameLen], "-")
	}
	return name
}

// AdoptedVMName is the name of the VirtualMachine single-host adoption
// creates for the hypervisor VM named name whose provider id is id:
// SanitizeVMName(name), or "vm-<id>" when that is empty.
func AdoptedVMName(name, id string) string {
	if n := SanitizeVMName(name); n != "" {
		return n
	}
	return "vm-" + id
}
