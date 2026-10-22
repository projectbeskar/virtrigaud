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

// Package cloneidentity gives a cloned VM its own cloud-init identity (R26).
//
// A full clone copies its source's disk, and with it cloud-init's record of
// the instance it last ran as (/var/lib/cloud/data/instance-id). If the
// clone's datasource still presents the SOURCE's instance-id, cloud-init sees
// the same instance and skips every per-instance module on the clone's first
// boot: the clone keeps the source's SSH host keys (the `ssh` module, whose
// ssh_deletekeys defaults to true, only regenerates them on a new instance)
// and the source's hostname — two VMs with one host identity, which defeats
// SSH host-key authentication.
//
// The helpers here compute the clone's identity — a fresh instance-id derived
// deterministically from the TARGET VirtualMachine (so a re-sent Clone renders
// the same seed), and the target's hostname — and rewrite cloud-init
// meta-data and user-data documents to carry it. They are pure (no I/O) and
// shared by the providers whose clones carry a cloud-init datasource the
// provider controls (libvirt's NoCloud seed ISO, vSphere's guestinfo keys).
//
// What a new instance-id does NOT do: cloud-init does not regenerate
// /etc/machine-id on a new instance. A clone of a VM whose machine-id is
// populated keeps it unless the source image was sealed (machine-id emptied,
// e.g. `cloud-init clean --machine-id`) before it was cloned.
package cloneidentity

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

const (
	// InstanceIDPrefix starts every instance-id a clone is given.
	InstanceIDPrefix = "virtrigaud-"

	// cloudConfigHeader is the first line of a #cloud-config user-data
	// document: the only user-data format whose hostname keys are rewritten.
	cloudConfigHeader = "#cloud-config"

	// metaKeyInstanceID, metaKeyLocalHostname and metaKeyHostname are the
	// meta-data keys a clone's identity is written to.
	metaKeyInstanceID    = "instance-id"
	metaKeyLocalHostname = "local-hostname"
	metaKeyHostname      = "hostname"

	// userKeyHostname and userKeyFQDN are the top-level #cloud-config keys
	// cloud-init's set_hostname/update_hostname modules prefer over the
	// meta-data's local-hostname.
	userKeyHostname = "hostname"
	userKeyFQDN     = "fqdn"

	// yamlStrTag is the YAML tag of a string scalar.
	yamlStrTag = "!!str"
)

// InstanceID returns the cloud-init instance-id of a clone made for target:
// InstanceIDPrefix + its UID, so the same VirtualMachine always renders the
// same instance-id (a re-sent Clone writes an identical seed) and a new
// VirtualMachine — even of the same namespace and name — is a new instance.
// fallback (the provider's name for the clone, e.g. its libvirt domain) is
// used only when the request carries no UID (a manager older than rc2).
func InstanceID(target contracts.ObjectIdentity, fallback string) string {
	if uid := strings.TrimSpace(target.UID); uid != "" {
		return InstanceIDPrefix + uid
	}
	return InstanceIDPrefix + fallback
}

// Customization is the part of a VMClone's spec.customization (the clone
// request's CustomizeJSON) a clone's cloud-init identity is built from.
type Customization struct {
	// Hostname is spec.customization.hostname ("" when unset).
	Hostname string
	// Domain is spec.customization.domain ("" when unset).
	Domain string
	// InlineUserData is spec.customization.userData.cloudInit.inline ("" when
	// unset): the clone's user-data, in place of its source's.
	InlineUserData string
	// SecretUserData is true when the user-data is given by a Secret
	// reference, which the provider cannot resolve (the manager does not
	// send Secret contents with a Clone).
	SecretUserData bool
	// Unapplied lists, sorted, the customization fields that are set but are
	// not part of a clone's cloud-init identity (networks, timeZone, sysprep,
	// ignition, ...): the caller reports them as not applied.
	Unapplied []string
}

// customizationJSON mirrors the JSON of v1beta1 VMCustomization for the
// fields a clone's identity uses.
type customizationJSON struct {
	Hostname string `json:"hostname"`
	Domain   string `json:"domain"`
	UserData *struct {
		CloudInit *struct {
			Inline    string          `json:"inline"`
			SecretRef json.RawMessage `json:"secretRef"`
		} `json:"cloudInit"`
		Ignition json.RawMessage `json:"ignition"`
	} `json:"userData"`
}

// appliedCustomizationKeys are the top-level customization keys
// ParseCustomization consumes.
var appliedCustomizationKeys = map[string]bool{"hostname": true, "domain": true, "userData": true}

// ParseCustomization reads a clone request's CustomizeJSON ("" is no
// customization).
func ParseCustomization(customizeJSON string) (Customization, error) {
	if strings.TrimSpace(customizeJSON) == "" {
		return Customization{}, nil
	}
	var c customizationJSON
	if err := json.Unmarshal([]byte(customizeJSON), &c); err != nil {
		return Customization{}, fmt.Errorf("parse clone customization: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(customizeJSON), &fields); err != nil {
		return Customization{}, fmt.Errorf("parse clone customization: %w", err)
	}
	out := Customization{Hostname: strings.TrimSpace(c.Hostname), Domain: strings.Trim(strings.TrimSpace(c.Domain), ".")}
	for k, v := range fields {
		if !appliedCustomizationKeys[k] && !isJSONEmpty(v) {
			out.Unapplied = append(out.Unapplied, k)
		}
	}
	if c.UserData != nil {
		if c.UserData.CloudInit != nil {
			out.InlineUserData = c.UserData.CloudInit.Inline
			out.SecretUserData = !isJSONEmpty(c.UserData.CloudInit.SecretRef)
		}
		if !isJSONEmpty(c.UserData.Ignition) {
			out.Unapplied = append(out.Unapplied, "userData.ignition")
		}
	}
	sort.Strings(out.Unapplied)
	return out, nil
}

// isJSONEmpty reports whether a raw JSON value is absent or an empty value
// (null, "", false, 0, [] or {}).
func isJSONEmpty(v json.RawMessage) bool {
	switch strings.TrimSpace(string(v)) {
	case "", "null", `""`, "false", "0", "[]", "{}":
		return true
	}
	return false
}

// Hostname returns the clone's hostname: the customization's hostname when
// set, else the target VirtualMachine's name, else targetName.
func Hostname(c Customization, target contracts.ObjectIdentity, targetName string) string {
	switch {
	case c.Hostname != "":
		return c.Hostname
	case target.Name != "":
		return target.Name
	}
	return targetName
}

// LocalHostname is the meta-data local-hostname for hostname in domain: the
// FQDN when a domain is given (cloud-init then sets the short hostname and
// the FQDN from it), else hostname.
func LocalHostname(hostname, domain string) string {
	if domain == "" {
		return hostname
	}
	return hostname + "." + domain
}

// RewriteMetaData returns the meta-data document src (YAML or JSON, the forms
// cloud-init accepts) with its instance-id set to instanceID and its
// local-hostname (and a top-level hostname key, if it has one) set to
// localHostname, adding the keys it lacks. Every other key, its order, and
// the document's comments are kept. An empty src yields a document of just
// those two keys. A src that is not a mapping is an error: the caller must
// not carry the source's identity forward.
func RewriteMetaData(src, instanceID, localHostname string) (string, error) {
	var doc yaml.Node
	if strings.TrimSpace(src) != "" {
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			return "", fmt.Errorf("parse cloud-init meta-data: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("cloud-init meta-data is not a mapping")
	}
	m := doc.Content[0]
	setMappingKey(m, metaKeyInstanceID, instanceID, true)
	setMappingKey(m, metaKeyLocalHostname, localHostname, true)
	setMappingKey(m, metaKeyHostname, localHostname, false)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("render cloud-init meta-data: %w", err)
	}
	return string(out), nil
}

// setMappingKey sets every top-level key of mapping m to the string value,
// keeping a quoted style the source used; with add, a key m lacks is
// appended.
func setMappingKey(m *yaml.Node, key, value string, add bool) {
	found := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		found = true
		style := m.Content[i+1].Style & (yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle)
		m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: value, Style: style}
	}
	if !found && add {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStrTag, Value: value})
	}
}

// RewriteUserDataHostname rewrites, in a #cloud-config user-data document,
// the top-level (column 0, block style) `hostname:` key to hostname and
// `fqdn:` key to hostname in domain (or, without a domain, in the domain of
// the FQDN it replaces). cloud-init's hostname modules prefer these keys over
// the meta-data's local-hostname, so a clone that kept its source's would
// boot with the source's name. Every other line is kept byte for byte. A
// document of any other kind (a shell script, MIME multi-part, a Jinja
// template) or a key whose value is not a plain one-line scalar is left
// untouched. changed reports whether a key was rewritten; skipped lists the
// keys found but left untouched.
func RewriteUserDataHostname(userData, hostname, domain string) (out string, changed bool, skipped []string) {
	if !strings.HasPrefix(userData, cloudConfigHeader) {
		return userData, false, nil
	}
	lines := strings.SplitAfter(userData, "\n")
	for i, line := range lines {
		content := strings.TrimRight(line, "\r\n")
		eol := line[len(content):]
		for _, key := range []string{userKeyHostname, userKeyFQDN} {
			rest, ok := strings.CutPrefix(content, key+":")
			if !ok {
				continue
			}
			old, ok := plainScalar(rest)
			if !ok {
				skipped = append(skipped, key)
				continue
			}
			value := hostname
			if key == userKeyFQDN {
				value = fqdnFor(hostname, domain, old)
			}
			lines[i] = key + ": " + quoteYAML(value) + eol
			changed = true
		}
	}
	return strings.Join(lines, ""), changed, skipped
}

// plainScalar returns the one-line scalar after a key's colon — unquoted, a
// trailing comment dropped — or false when the value is anything else: empty
// (a nested block), a block scalar, an anchor, alias, tag or flow collection.
func plainScalar(rest string) (string, bool) {
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false // "hostname:x" is not the key hostname
	}
	v := strings.TrimSpace(rest)
	if v == "" || strings.ContainsAny(v[:1], "|>&*!{[%@`#") {
		return "", false
	}
	if q := v[0]; q == '"' || q == '\'' {
		if end := strings.LastIndexByte(v, q); end > 0 {
			return v[1:end], true
		}
		return "", false
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, true
}

// fqdnFor is the FQDN of hostname: in domain when given, else in the domain
// of old (the FQDN it replaces), else hostname alone.
func fqdnFor(hostname, domain, old string) string {
	if domain == "" {
		if _, d, ok := strings.Cut(old, "."); ok {
			domain = d
		}
	}
	return LocalHostname(hostname, domain)
}

// quoteYAML renders s as a YAML double-quoted scalar (JSON string syntax is a
// subset of it).
func quoteYAML(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
