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

package v1beta1

import (
	"regexp"
	"strings"
	"testing"
)

// libvirtImagePathSchema returns the generated CRD schema node for
// VMImage spec.source.libvirt.path.
func libvirtImagePathSchema(t *testing.T) apiSchemaNode {
	t.Helper()
	return loadCRD(t, "infra.virtrigaud.io_vmimages.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema.
		prop(t, "spec").prop(t, "source").prop(t, "libvirt").prop(t, "path")
}

// TestLibvirtImagePathCRDPatternMatchesGo asserts the admission pattern
// controller-gen emitted for spec.source.libvirt.path is byte-identical to the
// exported LibvirtImagePathPattern the libvirt provider re-validates with, and
// that the field is bounded. A drift would let the apiserver admit a value the
// provider rejects (or the reverse).
func TestLibvirtImagePathCRDPatternMatchesGo(t *testing.T) {
	path := libvirtImagePathSchema(t)
	if path.Pattern != LibvirtImagePathPattern {
		t.Fatalf("CRD spec.source.libvirt.path pattern drifted from LibvirtImagePathPattern:\n crd: %s\n  go: %s",
			path.Pattern, LibvirtImagePathPattern)
	}
	if path.MaxLength == nil || *path.MaxLength != LibvirtImagePathMaxLength {
		t.Errorf("spec.source.libvirt.path maxLength = %v, want %d", path.MaxLength, LibvirtImagePathMaxLength)
	}
}

// TestLibvirtImagePathPattern_AcceptReject exercises the generated admission
// pattern (compiled with Go's RE2, as the apiserver's OpenAPI validator does)
// against legitimate image paths — which must keep being admitted on this
// released v1beta1 field — and against traversal / option-smuggling / control
// character payloads that must never be admitted.
func TestLibvirtImagePathPattern_AcceptReject(t *testing.T) {
	re := regexp.MustCompile(libvirtImagePathSchema(t).Pattern)

	accept := []string{
		"/var/lib/libvirt/images/ubuntu-22.04-cloudimg.qcow2",
		"/var/lib/libvirt/images/jammy-server-cloudimg-amd64.img",
		"/srv/images/win2022.vmdk",
		"/home/virt/.local/share/libvirt/images/fedora.qcow2", // session-mode default dir
		"/data/images/with space.qcow2",
		"/data/images/unicodé-✓.qcow2",
		"/data/images/v1.2.3..qcow2",
		"/data/.hidden-dir/x.qcow2",
		"/data/.../x.qcow2", // "..." is a legal name, not traversal
		"/data/..x/x.qcow2",
		"/data/./x.qcow2",
		"//var/lib/libvirt/images/x.qcow2", // repeated slashes are harmless
		"/x",
		"/data/images/$(not-a-command).qcow2", // inert: argv is shell-quoted end to end
	}
	reject := map[string]string{
		"empty":                     "",
		"relative":                  "var/lib/libvirt/images/x.qcow2",
		"dot relative":              "./x.qcow2",
		"root only":                 "/",
		"trailing slash":            "/var/lib/libvirt/images/",
		"dotdot segment":            "/var/lib/libvirt/images/../../../etc/shadow",
		"trailing dotdot":           "/var/lib/libvirt/images/..",
		"leading option":            "-o/etc/shadow",
		"segment starts with dash":  "/var/lib/libvirt/images/-rf",
		"first segment dash":        "/--help",
		"newline":                   "/var/lib/libvirt/images/x.qcow2\n/etc/shadow",
		"trailing newline":          "/var/lib/libvirt/images/x.qcow2\n",
		"carriage return":           "/var/lib/libvirt/images/x\r.qcow2",
		"tab":                       "/var/lib/libvirt/images/x\t.qcow2",
		"nul":                       "/var/lib/libvirt/images/x\x00.qcow2",
		"escape":                    "/var/lib/libvirt/images/x\x1b[0m.qcow2",
		"del":                       "/var/lib/libvirt/images/x\x7f.qcow2",
		"c1 control":                "/var/lib/libvirt/images/x\u0085.qcow2",
		"leading whitespace":        " /var/lib/libvirt/images/x.qcow2",
		"url instead of path":       "https://example.com/x.qcow2",
		"windows path":              `C:\images\x.qcow2`,
		"dotdot after double slash": "/var//../etc/shadow",
	}

	for _, p := range accept {
		if !re.MatchString(p) {
			t.Errorf("legitimate image path %q was rejected by the CRD pattern", p)
		}
	}
	for name, p := range reject {
		if re.MatchString(p) {
			t.Errorf("%s: payload %q was admitted by the CRD pattern", name, p)
		}
	}

	// PATH_MAX bound: the pattern itself is unbounded, the schema maxLength is not.
	long := "/" + strings.Repeat("a", LibvirtImagePathMaxLength)
	if !re.MatchString(long) {
		t.Fatalf("long but well-formed path should match the pattern (maxLength enforces the bound)")
	}
}
