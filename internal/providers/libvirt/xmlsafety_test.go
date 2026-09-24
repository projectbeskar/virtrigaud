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
	"encoding/xml"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestXMLEscape_Benign asserts xmlEscape is a no-op for values that contain
// none of the XML special characters — the overwhelming common case (bridge
// names, hostnames, MAC addresses) — which is what makes applying it
// uniformly behavior-preserving for legitimate input (issue #260).
func TestXMLEscape_Benign(t *testing.T) {
	tests := []string{
		"virbr0",
		"my-network",
		"52:54:00:aa:bb:cc",
		"vm-target-01",
		"/var/lib/libvirt/images/vm-disk.qcow2",
		"",
	}
	for _, in := range tests {
		assert.Equal(t, in, xmlEscape(in), "benign input %q must round-trip unchanged", in)
	}
}

// TestXMLEscape_Adversarial asserts every XML metacharacter this package
// relies on for single-quoted attributes and element text is escaped, and
// that round-tripping the escaped output through the standard library XML
// decoder recovers the exact original string — i.e. the escaping is both
// sufficient (no raw metacharacters survive) and correct (no information is
// lost or corrupted).
func TestXMLEscape_Adversarial(t *testing.T) {
	tests := []string{
		`x'/><disk type='block'><source dev='/dev/sda'/></disk><x a='`,
		`a&b`,
		`<`,
		`>`,
		`"`,
		`'`,
		"line1\nline2",
		`mix'"<>&of&everything'"`,
	}
	for _, in := range tests {
		got := xmlEscape(in)

		assert.NotContains(t, got, "'", "escaped output must not contain a raw single quote: %q -> %q", in, got)
		assert.NotContains(t, got, `"`, "escaped output must not contain a raw double quote: %q -> %q", in, got)
		assert.NotContains(t, got, "<", "escaped output must not contain a raw '<': %q -> %q", in, got)
		assert.NotContains(t, got, ">", "escaped output must not contain a raw '>': %q -> %q", in, got)
		// A bare '&' only appears as part of one of the entities xmlEscape itself
		// emits (&amp; &lt; &gt; &#39; &#34; ...), never standalone.
		for i, r := range got {
			if r != '&' {
				continue
			}
			require.Less(t, i+1, len(got), "trailing '&' in %q must be part of an entity", got)
		}

		// Round-trip: wrapping the escaped text as element content and decoding
		// it back through encoding/xml must recover the original string exactly.
		doc := "<r>" + got + "</r>"
		var decoded struct {
			Text string `xml:",chardata"`
		}
		require.NoError(t, xml.Unmarshal([]byte(doc), &decoded), "escaped output must be valid XML content: %q", got)
		assert.Equal(t, in, decoded.Text, "round-trip must recover the original value")

		// Also valid inside a single-quoted attribute.
		attrDoc := "<r a='" + got + "'/>"
		var decodedAttr struct {
			A string `xml:"a,attr"`
		}
		require.NoError(t, xml.Unmarshal([]byte(attrDoc), &decodedAttr), "escaped output must be valid inside a single-quoted attribute: %q", got)
		assert.Equal(t, in, decodedAttr.A, "attribute round-trip must recover the original value")
	}
}
