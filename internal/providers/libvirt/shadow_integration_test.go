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
	"strconv"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This is the ADR-0008 Tier-2 test: go-libvirt against a REAL libvirtd via the
// built-in test:///default driver (libvirt.TestDefault). It is what proves
// go-libvirt actually parses a live libvirtd's DomainGetXMLDesc into the right
// libvirtxml struct and that buildNativeDescribe produces the expected projection
// — the thing a fake dialer cannot prove.
//
// It connects go-libvirt DIRECTLY to the local libvirtd unix socket (NOT through
// the SSH tunnel/holder — that lifecycle is covered by golibvirt_test.go's fake
// dialer), then opens the test driver, which needs no KVM, no qemu, and no
// privilege — exactly why ADR-0008's toolchain spike proved it runs in a plain
// unprivileged libvirt-daemon-system container on stock CI runners.
//
// It auto-SKIPS (never fails) when no libvirtd socket is reachable, so `make test`
// on a machine without libvirtd is unaffected; `make test-libvirt-integration`
// runs it explicitly, and a CI job wires a libvirt-daemon-system service (see the
// Makefile target + docs/libvirt-shadow-reads.md). Deliberately NOT build-tag
// gated: build-tagged code rots uncompiled (PR #291 finding B4), so this always
// compiles and only its execution is conditional.

// connectTestDriver dials the local libvirtd and opens test:///default, skipping
// the test if either step fails (no daemon, no socket permission, no test driver).
func connectTestDriver(t *testing.T) *golibvirt.Libvirt {
	t.Helper()
	lv := golibvirt.NewWithDialer(dialers.NewLocal())
	if err := lv.ConnectToURI(golibvirt.TestDefault); err != nil {
		t.Skipf("skipping: no reachable libvirtd test:///default driver (%v); run `make test-libvirt-integration` on a host with libvirt-daemon-system", err)
	}
	t.Cleanup(func() { _ = lv.Disconnect() })
	return lv
}

// TestBuildNativeDescribe_TestDriver drives buildNativeDescribe against the test
// driver's built-in "test" domain and asserts the projection the shadow comparator
// consumes. This is the go-libvirt + libvirtxml parse proof.
func TestBuildNativeDescribe_TestDriver(t *testing.T) {
	lv := connectTestDriver(t)

	resp, err := buildNativeDescribe(lv, "test")
	require.NoError(t, err, "buildNativeDescribe against the test driver's default domain")

	assert.True(t, resp.Exists, "the test driver's default domain exists")
	assert.Equal(t, "On", resp.PowerState, "the test driver's default domain runs")

	// Identity + config come from DomainGetXMLDesc parsed through libvirtxml.
	assert.Equal(t, "test", resp.ProviderRaw["name"], "domain name parsed from XML")
	assert.NotEmpty(t, resp.ProviderRaw["uuid"], "uuid parsed from XML")
	assert.Equal(t, resp.PowerState, resp.ProviderRaw["power_state_mapped"])
	assert.NotEmpty(t, resp.ProviderRaw["vcpu"], "vcpu parsed from XML")
	assert.NotEmpty(t, resp.ProviderRaw["memory_mib"], "memory parsed from XML (KiB->MiB guard applied)")
}

// TestBuildNativeDescribe_NotFound verifies the not-found path: a missing domain
// yields Exists=false with NO error (so the shadow harness meters a clean compare,
// not a spurious error), via go-libvirt's IsNotFound classifier.
func TestBuildNativeDescribe_NotFound(t *testing.T) {
	lv := connectTestDriver(t)

	resp, err := buildNativeDescribe(lv, "no-such-domain-1a2b3c")
	require.NoError(t, err, "a missing domain is not an error")
	assert.False(t, resp.Exists)
	assert.Equal(t, "Off", resp.PowerState)
}

// TestNativeVsVirshParity_TestDriver is the shadow comparison end-to-end against a
// real daemon: it builds the native projection from go-libvirt and a synthetic
// "virsh" projection from the SAME domain's data, and asserts compareDescribe finds
// zero semantic divergence — the shape of evidence the D5 soak accumulates in
// production, exercised here in CI.
func TestNativeVsVirshParity_TestDriver(t *testing.T) {
	lv := connectTestDriver(t)

	native, err := buildNativeDescribe(lv, "test")
	require.NoError(t, err)

	// Build the virsh-shaped projection from the same live values, keyed the way
	// `virsh dominfo` keys them (Name/UUID/CPU(s)/Max memory in KiB). memory_mib is
	// MiB in native; convert back to KiB for the virsh side to prove the unit
	// canonicalization holds against real data.
	memMiB := native.ProviderRaw["memory_mib"]
	kib := "0"
	if memMiB != "" {
		if n, ok := firstInt(memMiB); ok {
			kib = strconv.FormatInt(n*1024, 10) + " KiB"
		}
	}
	virsh := virshResp(native.PowerState, map[string]string{
		"Name":       native.ProviderRaw["name"],
		"UUID":       native.ProviderRaw["uuid"],
		"CPU(s)":     native.ProviderRaw["vcpu"],
		"Max memory": kib,
	})

	diverged := compareDescribe(virsh, native)
	assert.Empty(t, diverged, "native and virsh projections of the same live domain must not diverge; got %v", diverged)
}
