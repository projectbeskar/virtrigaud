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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// These tests pin the libvirt domain-ownership fix: Create stamps the
// requesting VirtualMachine's identity into a new domain's <metadata>, binds to
// an existing same-named domain ONLY when that stamp records the requester's
// UID, and otherwise fails closed with a non-retryable Conflict — on both the
// single-host and the clustered (ADR-0007) create paths. Names virsh would
// resolve as a domain ID/UUID are rejected before any host is touched.

var (
	ownerTeamA = contracts.ObjectIdentity{UID: "0b8f5e0e-7a53-4f6b-9a0e-1d2c3b4a5f60", Namespace: "team-a", Name: "web"}
	ownerTeamB = contracts.ObjectIdentity{UID: "c4d1a9f2-3e6b-4c7d-8e9f-0a1b2c3d4e5f", Namespace: "team-b", Name: "web"}

	uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// stampedDomainXML returns a minimal `virsh dumpxml` document carrying owner's
// stamp exactly as Create renders it.
func stampedDomainXML(name string, owner contracts.ObjectIdentity) string {
	return fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n%s  <memory unit='KiB'>1048576</memory>\n</domain>\n",
		name, "11111111-2222-4333-8444-555555555555", renderOwnerMetadataXML(owner))
}

// unstampedDomainXML is a domain VirtRigaud never created (no owner stamp), with
// another tool's metadata present to prove it is not mistaken for a stamp.
func unstampedDomainXML(name string) string {
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <uuid>99999999-8888-4777-8666-555555555555</uuid>
  <metadata>
    <libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0">
      <libosinfo:os id="http://ubuntu.com/ubuntu/22.04"/>
    </libosinfo:libosinfo>
  </metadata>
</domain>
`, name)
}

// --- pure helpers -------------------------------------------------------------

func TestRenderOwnerMetadataXML_EscapesAndRoundTrips(t *testing.T) {
	assert.Empty(t, renderOwnerMetadataXML(contracts.ObjectIdentity{}), "no UID -> nothing to stamp")
	assert.Empty(t, renderOwnerMetadataXML(contracts.ObjectIdentity{Namespace: "ns", Name: "n"}),
		"namespace/name without a UID prove nothing and are not stamped")

	hostile := contracts.ObjectIdentity{
		UID:       `u'/><disk type='block'><source dev='/dev/sda'/></disk><x a='`,
		Namespace: `ns"<&>`,
		Name:      `n'm`,
	}
	block := renderOwnerMetadataXML(hostile)
	assert.NotContains(t, block, "<disk", "an owner value must never splice an element into the domain XML")
	assert.Contains(t, block, "xmlns:"+ownerMetadataPrefix+"='"+ownerMetadataNamespaceURI+"'")

	// The stamped document is well-formed and the values read back verbatim.
	owners, err := domainOwners(stampedDomainXML("web", hostile))
	require.NoError(t, err)
	require.Equal(t, []contracts.ObjectIdentity{hostile}, owners)

	// libvirt's own parser (libvirtxml) accepts it and keeps the stamp in <metadata>.
	d, err := parseDomainLibvirtxml(stampedDomainXML("web", ownerTeamA))
	require.NoError(t, err)
	require.NotNil(t, d.Metadata)
	assert.Contains(t, d.Metadata.XML, ownerMetadataNamespaceURI)
}

func TestScanOwnerElements(t *testing.T) {
	t.Run("matches by namespace URI, not prefix, and wherever xmlns is declared", func(t *testing.T) {
		doc := `<domain><name>web</name><metadata xmlns:vr="` + ownerMetadataNamespaceURI + `">` +
			`<vr:owner uid="u1" namespace="a" name="web"></vr:owner></metadata></domain>`
		owners, err := domainOwners(doc)
		require.NoError(t, err)
		assert.Equal(t, []contracts.ObjectIdentity{{UID: "u1", Namespace: "a", Name: "web"}}, owners)
	})
	t.Run("ignores other namespaces, unprefixed owner, and stamps outside /domain/metadata", func(t *testing.T) {
		doc := `<domain><name>web</name>` +
			`<metadata><x:owner xmlns:x="https://example.com/other" uid="evil"/><owner uid="evil2"/></metadata>` +
			`<devices><v:owner xmlns:v="` + ownerMetadataNamespaceURI + `" uid="evil3"/></devices>` +
			`<description>&lt;virtrigaud:owner uid="evil4"/&gt;</description></domain>`
		owners, err := domainOwners(doc)
		require.NoError(t, err)
		assert.Empty(t, owners)
	})
	t.Run("libosinfo-only metadata has no owner", func(t *testing.T) {
		owners, err := domainOwners(unstampedDomainXML("web"))
		require.NoError(t, err)
		assert.Empty(t, owners)
	})
	t.Run("reports every stamp", func(t *testing.T) {
		stamp := `<v:owner xmlns:v="` + ownerMetadataNamespaceURI + `" uid="%s"/>`
		doc := `<domain><metadata>` + fmt.Sprintf(stamp, "u1") + fmt.Sprintf(stamp, "u2") + `</metadata></domain>`
		owners, err := domainOwners(doc)
		require.NoError(t, err)
		assert.Len(t, owners, 2)
	})
	t.Run("malformed or non-domain documents are errors", func(t *testing.T) {
		for _, doc := range []string{`<domain><metadata>`, `<network><name>x</name></network>`, `not xml <`} {
			_, err := domainOwners(doc)
			assert.Error(t, err, "%q", doc)
		}
	})
	t.Run("a repeated owner attribute is refused, not resolved last-wins", func(t *testing.T) {
		doc := `<domain><metadata><v:owner xmlns:v="` + ownerMetadataNamespaceURI + `" uid="victim" uid="attacker"/></metadata></domain>`
		_, err := domainOwners(doc)
		assert.Error(t, err)
		assert.False(t, requesterOwnsDomain(contracts.ObjectIdentity{UID: "attacker"}, nil))
	})
	t.Run("a second root element is refused", func(t *testing.T) {
		stamp := `<v:owner xmlns:v="` + ownerMetadataNamespaceURI + `" uid="u1"/>`
		doc := `<domain><name>web</name></domain><domain><metadata>` + stamp + `</metadata></domain>`
		_, err := domainOwners(doc)
		assert.Error(t, err)
	})
}

func TestRequesterOwnsDomain(t *testing.T) {
	cases := []struct {
		name      string
		requester contracts.ObjectIdentity
		recorded  []contracts.ObjectIdentity
		want      bool
	}{
		{"same uid", ownerTeamA, []contracts.ObjectIdentity{ownerTeamA}, true},
		{"different uid, same namespace/name", ownerTeamA,
			[]contracts.ObjectIdentity{{UID: ownerTeamB.UID, Namespace: ownerTeamA.Namespace, Name: ownerTeamA.Name}}, false},
		{"other tenant", ownerTeamB, []contracts.ObjectIdentity{ownerTeamA}, false},
		{"no stamp", ownerTeamA, nil, false},
		{"requester without uid (old manager)", contracts.ObjectIdentity{}, []contracts.ObjectIdentity{{}}, false},
		{"requester without uid vs stamped", contracts.ObjectIdentity{Namespace: "team-a", Name: "web"},
			[]contracts.ObjectIdentity{ownerTeamA}, false},
		{"stamp with empty uid", ownerTeamA, []contracts.ObjectIdentity{{Namespace: "team-a", Name: "web"}}, false},
		{"ambiguous: two stamps incl. ours", ownerTeamA, []contracts.ObjectIdentity{ownerTeamA, ownerTeamB}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, requesterOwnsDomain(tc.requester, tc.recorded))
		})
	}
}

func TestStripOwnerMetadata_SplicesOnlyTheStamp(t *testing.T) {
	src := `<domain type='kvm'>
  <name>web</name>
  <metadata>
    <libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"><libosinfo:os id="x"/></libosinfo:libosinfo>
    <virtrigaud:owner xmlns:virtrigaud="` + ownerMetadataNamespaceURI + `" uid="u1" namespace="a" name="web"/>
  </metadata>
</domain>`
	out, err := stripOwnerMetadata(src)
	require.NoError(t, err)
	owners, err := domainOwners(out)
	require.NoError(t, err)
	assert.Empty(t, owners, "the stamp must be gone")
	assert.Contains(t, out, `<libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"><libosinfo:os id="x"/></libosinfo:libosinfo>`,
		"other tools' metadata must be preserved byte-for-byte")
	assert.Equal(t, strings.Replace(src,
		`<virtrigaud:owner xmlns:virtrigaud="`+ownerMetadataNamespaceURI+`" uid="u1" namespace="a" name="web"/>`, "", 1), out,
		"exactly the stamp's bytes are removed; nothing is re-serialized")

	unchanged, err := stripOwnerMetadata(unstampedDomainXML("web"))
	require.NoError(t, err)
	assert.Equal(t, unstampedDomainXML("web"), unchanged)
}

// TestRewriteDomainXMLForClone_DropsSourceOwner proves a clone never inherits
// (and so never falsely claims) the source VirtualMachine's owner stamp.
func TestRewriteDomainXMLForClone_DropsSourceOwner(t *testing.T) {
	const srcDisk = "/var/lib/libvirt/images/web-disk.qcow2"
	src := `<domain type='kvm'>
  <name>web</name>
  <uuid>11111111-2222-4333-8444-555555555555</uuid>
` + renderOwnerMetadataXML(ownerTeamA) + `  <devices>
    <disk type='file' device='disk'><source file='` + srcDisk + `'/></disk>
  </devices>
</domain>`
	out, _, _, err := rewriteDomainXMLForClone(src, "web-clone", srcDisk, "/var/lib/libvirt/images/web-clone-disk.qcow2")
	require.NoError(t, err)
	owners, err := domainOwners(out)
	require.NoError(t, err)
	assert.Empty(t, owners, "the clone must not carry the source VM's owner stamp")
	assert.NotContains(t, out, ownerTeamA.UID)
}

func TestAmbiguousDomainNameError(t *testing.T) {
	rejected := []string{
		"0", "12", "007", "1234567890", " 12", "+5", "-0",
		"1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		"1B4E28BA-2FA1-11D2-883F-0016D3CCA427",
		"1b4e28ba2fa111d2883f0016d3cca427",
		"1b4e28ba-2fa1-11d2-883f0016d3cca427",
	}
	for _, name := range rejected {
		err := ambiguousDomainNameError(name)
		require.Error(t, err, "%q must be rejected", name)
		var pe *contracts.ProviderError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, contracts.ErrorTypeInvalidSpec, pe.Type, "%q", name)
		assert.False(t, pe.IsRetryable(), "%q", name)
	}

	accepted := []string{
		"web", "web-01", "vm-12", "12a", "a12", "db-1b4e28ba",
		"deadbeef",
		"1b4e28ba-2fa1-11d2-883f-0016d3cca42",   // 31 hex digits
		"1b4e28ba-2fa1-11d2-883f-0016d3cca4270", // 33 hex digits
		"1b4e28ba-2fa1-11d2-883f-0016d3cca42g",  // non-hex
		"",                                      // emptiness is checked elsewhere
	}
	for _, name := range accepted {
		assert.NoError(t, ambiguousDomainNameError(name), "%q must be accepted", name)
	}
}

// --- create-path domain XML ---------------------------------------------------

// localTestVirshProvider returns a VirshProvider on a LOCAL (non-ssh) URI, so
// every command execs a local binary: `virsh` from PATH (the fake installed by
// installOwnershipFakeVirsh when a test needs one) and "!" commands directly.
func localTestVirshProvider() *VirshProvider {
	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu:///system"}})
	vp.uri = "qemu:///system"
	return vp
}

func TestGenerateDomainXMLWithStorage_StampsOwnerWithRandomUUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: the /dev/kvm probe execs test(1)")
	}
	p := &Provider{}
	vp := localTestVirshProvider()
	ctx := context.Background()

	hostile := contracts.ObjectIdentity{UID: ownerTeamA.UID, Namespace: `team-a'"<>&`, Name: "web"}
	req := contracts.CreateRequest{Name: "web", Owner: hostile}

	x1, err := p.generateDomainXMLWithStorage(ctx, vp, req, "/var/lib/libvirt/images/web-disk.qcow2.qcow2", "")
	require.NoError(t, err)
	x2, err := p.generateDomainXMLWithStorage(ctx, vp, req, "/var/lib/libvirt/images/web-disk.qcow2.qcow2", "")
	require.NoError(t, err)

	owners, err := domainOwners(x1)
	require.NoError(t, err)
	require.Equal(t, []contracts.ObjectIdentity{hostile}, owners, "the new domain must carry the requester's (escaped) identity")

	d1, err := parseDomainLibvirtxml(x1)
	require.NoError(t, err, "the generated domain XML must stay well-formed")
	d2, err := parseDomainLibvirtxml(x2)
	require.NoError(t, err)
	assert.Regexp(t, uuidV4RE, d1.UUID, "domain UUID must be RFC 4122 v4")
	assert.Regexp(t, uuidV4RE, d2.UUID)
	assert.NotEqual(t, d1.UUID, d2.UUID, "domain UUIDs must not repeat across creates")
	assert.NotContains(t, x1, "550e8400-e29b-41d4-a716-", "the old predictable UUID prefix must be gone")

	// No owner -> no <metadata> at all (and still a valid document).
	x3, err := p.generateDomainXMLWithStorage(ctx, vp, contracts.CreateRequest{Name: "web"}, "/d.qcow2", "")
	require.NoError(t, err)
	assert.NotContains(t, x3, "<metadata>")
	_, err = parseDomainLibvirtxml(x3)
	require.NoError(t, err)
}

// --- Create against a fake virsh ------------------------------------------------

// installOwnershipFakeVirsh puts a fake `virsh` on PATH that serves
// `list --all` and `dumpxml <name>` from domains (name -> dumpxml document),
// logs every invocation, and FAILS any other command — so a test proves the
// create path ran nothing but read-only queries before deciding. It returns
// the log path.
func installOwnershipFakeVirsh(t *testing.T, domains map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell shim not applicable on Windows")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "virsh.log")

	var list strings.Builder
	list.WriteString(" Id   Name                 State\n")
	list.WriteString("------------------------------------\n")
	i := 1
	for name := range domains {
		fmt.Fprintf(&list, " %-4d %-20s running\n", i, name)
		i++
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dom-"+name+".xml"), []byte(domains[name]), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "list.txt"), []byte(list.String()), 0o600))

	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_VIRSH_DIR/virsh.log"
if [ "$1" = "-c" ]; then shift 2; fi
case "$1" in
  list) cat "$FAKE_VIRSH_DIR/list.txt" ;;
  dumpxml)
    f="$FAKE_VIRSH_DIR/dom-$2.xml"
    if [ -f "$f" ]; then cat "$f"; else echo "error: failed to get domain '$2'" >&2; exit 1; fi ;;
  *) echo "fake virsh: unexpected mutating command: $*" >&2; exit 97 ;;
esac
`
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("FAKE_VIRSH_DIR", dir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// virshLog returns the fake virsh invocations (without the -c <uri> prefix).
func virshLog(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath) //nolint:gosec // test reads its own fixture log
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, strings.TrimPrefix(l, "-c qemu:///system "))
		}
	}
	return out
}

// requireConflictNoBind asserts err is the non-retryable Conflict, names only
// the requested domain (never the other owner), and that nothing but the
// read-only list/dumpxml ran.
func requireConflictNoBind(t *testing.T, resp contracts.CreateResponse, err error, logPath string) {
	t.Helper()
	require.Error(t, err)
	assert.Empty(t, resp.ID, "a refused create must not return an ID to bind")
	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, contracts.ErrorTypeConflict, pe.Type)
	assert.False(t, pe.IsRetryable(), "an ownership conflict is not resolved by retrying")
	assert.Contains(t, pe.Message, `libvirt domain "web" already exists`)
	assert.Contains(t, pe.Message, "adoption flow")
	for _, leak := range []string{ownerTeamA.UID, ownerTeamA.Namespace} {
		assert.NotContains(t, pe.Message, leak, "the status-bound message must not disclose the other owner")
	}
	assert.Equal(t, []string{"list --all", "dumpxml web"}, virshLog(t, logPath),
		"only read-only queries may run before refusing")
}

func TestCreate_ExistingDomain_Ownership(t *testing.T) {
	ctx := context.Background()

	t.Run("owned by the requester is an idempotent success", func(t *testing.T) {
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": stampedDomainXML("web", ownerTeamA)})
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
		require.NoError(t, err)
		assert.Equal(t, "web", resp.ID)
		assert.Equal(t, []string{"list --all", "dumpxml web"}, virshLog(t, logPath))
	})

	t.Run("owned by another tenant is refused", func(t *testing.T) {
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": stampedDomainXML("web", ownerTeamA)})
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web", Owner: ownerTeamB})
		requireConflictNoBind(t, resp, err, logPath)
	})

	t.Run("no owner metadata is refused", func(t *testing.T) {
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": unstampedDomainXML("web")})
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web", Owner: ownerTeamB})
		requireConflictNoBind(t, resp, err, logPath)
	})

	t.Run("request without owner (old manager) is refused", func(t *testing.T) {
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": stampedDomainXML("web", ownerTeamA)})
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web"})
		requireConflictNoBind(t, resp, err, logPath)
	})

	t.Run("unparseable domain XML is refused", func(t *testing.T) {
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": "<domain><name>web"})
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
		requireConflictNoBind(t, resp, err, logPath)
	})

	t.Run("dumpxml failure is retryable, not a bind", func(t *testing.T) {
		// Listed but not dumpable (e.g. it vanished between list and dumpxml).
		logPath := installOwnershipFakeVirsh(t, map[string]string{"web": ""})
		require.NoError(t, os.Remove(filepath.Join(os.Getenv("FAKE_VIRSH_DIR"), "dom-web.xml")))
		p := &Provider{virshProvider: localTestVirshProvider()}

		resp, err := p.Create(ctx, contracts.CreateRequest{Name: "web", Owner: ownerTeamA})
		require.Error(t, err)
		assert.Empty(t, resp.ID)
		var pe *contracts.ProviderError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, contracts.ErrorTypeRetryable, pe.Type)
		assert.Equal(t, []string{"list --all", "dumpxml web"}, virshLog(t, logPath))
	})
}

// TestCreate_AbsentDomainProceedsToCreate proves the ownership check does not
// block a genuinely new VM — including from an older manager that sends no
// owner: with no same-named domain the create pipeline runs (here it then stops
// at the fake's first mutating command, surfacing as the usual retryable error,
// never a Conflict).
func TestCreate_AbsentDomainProceedsToCreate(t *testing.T) {
	for name, owner := range map[string]contracts.ObjectIdentity{"with owner": ownerTeamA, "without owner": {}} {
		t.Run(name, func(t *testing.T) {
			logPath := installOwnershipFakeVirsh(t, map[string]string{"other": stampedDomainXML("other", ownerTeamB)})
			p := &Provider{virshProvider: localTestVirshProvider()}

			_, err := p.Create(context.Background(), contracts.CreateRequest{Name: "web", Owner: owner})
			var pe *contracts.ProviderError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, contracts.ErrorTypeRetryable, pe.Type, "an absent domain is created, not refused")

			calls := virshLog(t, logPath)
			require.Greater(t, len(calls), 1, "the create pipeline must run past the existence check")
			assert.Equal(t, "list --all", calls[0])
			assert.NotContains(t, calls, "dumpxml web", "no ownership lookup when nothing of that name exists")
		})
	}
}

func TestCreate_AmbiguousNameRejectedBeforeTouchingHost(t *testing.T) {
	for _, name := range []string{"12", "1b4e28ba-2fa1-11d2-883f-0016d3cca427"} {
		t.Run(name, func(t *testing.T) {
			logPath := installOwnershipFakeVirsh(t, map[string]string{"web": stampedDomainXML("web", ownerTeamA)})
			p := &Provider{virshProvider: localTestVirshProvider()}

			_, err := p.Create(context.Background(), contracts.CreateRequest{Name: name, Owner: ownerTeamA})
			var pe *contracts.ProviderError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, contracts.ErrorTypeInvalidSpec, pe.Type)
			assert.Empty(t, virshLog(t, logPath), "no virsh command may run for an ambiguous name")
		})
	}
}

// TestCreate_Clustered_OwnershipAppliesOnTargetHost drives the PRODUCTION
// clustered path (createClustered -> createOnLeasedHost -> createVM, no
// createOnHostFn seam) against a leased host whose virsh is the fake, proving
// the ownership rule is enforced on the scheduled host too.
func TestCreate_Clustered_OwnershipAppliesOnTargetHost(t *testing.T) {
	logPath := installOwnershipFakeVirsh(t, map[string]string{"web": stampedDomainXML("web", ownerTeamA)})
	vc := newClusteredVirshConn("host-a", localTestVirshProvider(), nil)
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	p, _ := newClusteredProviderForTest(t, inv, func(_ context.Context, _ hostsecret.Host) (hostconn.Conn, error) { return vc, nil })
	require.Nil(t, p.createOnHostFn, "must exercise the production create-on-host path")

	resp, err := p.Create(context.Background(), contracts.CreateRequest{Name: "web", TargetHostID: "host-a", Owner: ownerTeamB})
	requireConflictNoBind(t, resp, err, logPath)

	resp, err = p.Create(context.Background(), contracts.CreateRequest{Name: "web", TargetHostID: "host-a", Owner: ownerTeamA})
	require.NoError(t, err, "the owner's retried create on the same host is idempotent")
	assert.Equal(t, "web", resp.ID)
}
