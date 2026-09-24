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

package vsphere

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// --- pure helpers ------------------------------------------------------------

func TestTemplateRefError(t *testing.T) {
	valid := []string{
		"ubuntu-22.04-server-template", "vm-web", "rhel 8.8 template", "tmpl*", // bare names (compared exactly)
		"/DC0/vm/templates/ubuntu", "templates/ubuntu", "/DC0/vm/a%2fb", // inventory paths
	}
	for _, ref := range valid {
		assert.NoError(t, templateRefError(ref), "template reference %q must be accepted", ref)
	}

	invalid := []string{
		"", "  ",
		"vm-12", "vm-0", // MOID-shaped
		"VirtualMachine:vm-12", `a\b`, "50%", // reserved characters in a bare name
		"/DC0/vm/../prod", "/DC0/vm/./x", "DC0//x", "/DC0/vm/tmpl/", // bad path elements
		"/DC0/vm/vm-7",               // MOID-shaped last path element
		"/DC0/vm/a:b", `/DC0/vm/a\b`, // reserved characters in a path
	}
	for _, ref := range invalid {
		requireCode(t, templateRefError(ref), codes.InvalidArgument)
	}
}

func TestSelectTemplate(t *testing.T) {
	tmplA := templateCandidate{ref: vmRef("vm-10"), isTemplate: true}
	tmplB := templateCandidate{ref: vmRef("vm-11"), isTemplate: true}
	regular := templateCandidate{ref: vmRef("vm-20"), isTemplate: false}

	got, found, err := selectTemplate("t", []templateCandidate{tmplA})
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, tmplA.ref, got)

	got, found, err = selectTemplate("t", []templateCandidate{regular, tmplA, regular})
	require.NoError(t, err, "same-named regular VMs are ignored, never block a real template")
	assert.True(t, found)
	assert.Equal(t, tmplA.ref, got)

	_, found, err = selectTemplate("t", []templateCandidate{tmplA, tmplB})
	requireCode(t, err, codes.InvalidArgument)
	assert.False(t, found, "two templates with the name are ambiguous")

	_, found, err = selectTemplate("t", []templateCandidate{regular})
	requireCode(t, err, codes.InvalidArgument)
	assert.False(t, found, "a regular VM is never a clone source")

	_, found, err = selectTemplate("t", nil)
	require.NoError(t, err)
	assert.False(t, found)
}

// --- vcsim helpers -----------------------------------------------------------

// cloneVMInto clones VM src (by MOID) into folder as name using govmomi
// directly (no VirtRigaud logic) and returns the new MOID.
func cloneVMInto(t *testing.T, p *Provider, src string, folder *object.Folder, name string) string {
	t.Helper()
	ctx := context.Background()
	cluster, err := p.finder.ClusterComputeResource(ctx, simCluster)
	require.NoError(t, err)
	pool, err := cluster.ResourcePool(ctx)
	require.NoError(t, err)
	poolRef := pool.Reference()

	task, err := object.NewVirtualMachine(p.client.Client, vmRef(src)).Clone(ctx, folder, name,
		types.VirtualMachineCloneSpec{Location: types.VirtualMachineRelocateSpec{Pool: &poolRef}})
	require.NoError(t, err)
	info, err := task.WaitForResult(ctx, nil)
	require.NoError(t, err)
	ref, ok := info.Result.(types.ManagedObjectReference)
	require.True(t, ok)
	return ref.Value
}

// subFolder creates a VM folder named name under DC0/vm.
func subFolder(t *testing.T, p *Provider, name string) *object.Folder {
	t.Helper()
	f, err := defaultVMFolder(t, p).CreateFolder(context.Background(), name)
	require.NoError(t, err)
	return f
}

// numCPU reads the configured vCPU count of VM id (vcsim copies it from the
// clone source, which lets a test tell which object was cloned).
func numCPU(t *testing.T, p *Provider, id string) int32 {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.numCPU"}, &vm))
	return vm.Config.Hardware.NumCPU
}

func setNumCPU(t *testing.T, p *Provider, id string, n int32) {
	t.Helper()
	ctx := context.Background()
	vm := object.NewVirtualMachine(p.client.Client, vmRef(id))
	if task, err := vm.PowerOff(ctx); err == nil {
		_ = task.Wait(ctx)
	}
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{NumCPUs: n})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

func createFromTemplate(p *Provider, name, templateRef string) (*providerv1.CreateResponse, error) {
	return p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      name,
		ImageJson: fmt.Sprintf(`{"TemplateName":%q}`, templateRef),
		Owner:     wireOwner(ownerTeamA),
	})
}

// requireNoVMNamed asserts that no VM named name exists anywhere in DC0.
func requireNoVMNamed(t *testing.T, p *Provider, name string) {
	t.Helper()
	candidates, err := p.templateCandidates(context.Background(), name)
	require.NoError(t, err)
	assert.Empty(t, candidates, "no VM named %q may have been created", name)
}

// --- vcsim: Create clone source ------------------------------------------------

func TestCreate_TemplateSource_AcceptsRealTemplates(t *testing.T) {
	p, _ := newOwnershipSim(t) // marks simTemplate as a template

	byName, err := createFromTemplate(p, "by-name", simTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, byName.Id)

	byAbsPath, err := createFromTemplate(p, "by-abs-path", "/DC0/vm/"+simTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, byAbsPath.Id)

	folder := subFolder(t, p, "templates")
	nested := cloneVMInto(t, p, seededVMID(t, p, simTemplate), folder, "nested-tmpl")
	markAsTemplate(t, p, nested)

	byRelPath, err := createFromTemplate(p, "by-rel-path", "templates/nested-tmpl")
	require.NoError(t, err, "a path relative to the datacenter VM folder resolves")
	assert.NotEmpty(t, byRelPath.Id)

	byNestedName, err := createFromTemplate(p, "by-nested-name", "nested-tmpl")
	require.NoError(t, err, "a bare name finds a template in a nested folder")
	assert.NotEmpty(t, byNestedName.Id)
}

// TestCreate_TemplateSource_RejectsRegularVM is the reported High: a VMImage
// templateName naming another tenant's (or a production) VM used to full-clone
// it. A regular VM — running or powered off — is never a clone source.
func TestCreate_TemplateSource_RejectsRegularVM(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	// Running regular VM.
	_, err := createFromTemplate(p, "steal-running", simUnstamped)
	requireCode(t, err, codes.InvalidArgument)
	assert.NotContains(t, err.Error(), seededVMID(t, p, simUnstamped), "the message names only the requested template")
	requireNoVMNamed(t, p, "steal-running")

	// Powered-off regular VM.
	victim := object.NewVirtualMachine(p.client.Client, vmRef(seededVMID(t, p, simUnstamped)))
	task, err := victim.PowerOff(ctx)
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
	_, err = createFromTemplate(p, "steal-off", simUnstamped)
	requireCode(t, err, codes.InvalidArgument)
	requireNoVMNamed(t, p, "steal-off")

	// Regular VM by inventory path.
	_, err = createFromTemplate(p, "steal-path", "/DC0/vm/"+simUnstamped)
	requireCode(t, err, codes.InvalidArgument)
	requireNoVMNamed(t, p, "steal-path")
}

// TestCreate_TemplateSource_SameNamedRegularVMIsIgnored: a regular VM named
// like a shared template (e.g. a tenant's VirtualMachine) neither blocks the
// template nor is cloned instead of it.
func TestCreate_TemplateSource_SameNamedRegularVMIsIgnored(t *testing.T) {
	p, _ := newOwnershipSim(t)
	seed := seededVMID(t, p, simTemplate)

	golden := cloneVMInto(t, p, seed, defaultVMFolder(t, p), "golden")
	setNumCPU(t, p, golden, 3)
	markAsTemplate(t, p, golden)

	impostor := cloneVMInto(t, p, seed, subFolder(t, p, "tenant-b"), "golden")
	require.NotEqual(t, int32(3), numCPU(t, p, impostor), "setup: the regular VM is distinguishable")

	resp, err := createFromTemplate(p, "consumer", "golden")
	require.NoError(t, err)
	assert.Equal(t, int32(3), numCPU(t, p, resp.Id), "the real template, not the same-named VM, was cloned")
}

// TestCreate_TemplateSource_AmbiguousFailsClosed: two templates with the same
// name are refused by bare name (never "the first one"); an inventory path
// disambiguates.
func TestCreate_TemplateSource_AmbiguousFailsClosed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	seed := seededVMID(t, p, simTemplate)

	markAsTemplate(t, p, cloneVMInto(t, p, seed, defaultVMFolder(t, p), "dup-tmpl"))
	markAsTemplate(t, p, cloneVMInto(t, p, seed, subFolder(t, p, "other"), "dup-tmpl"))

	_, err := createFromTemplate(p, "ambiguous", "dup-tmpl")
	requireCode(t, err, codes.InvalidArgument)
	requireNoVMNamed(t, p, "ambiguous")

	_, err = createFromTemplate(p, "disambiguated", "/DC0/vm/other/dup-tmpl")
	require.NoError(t, err)
}

func TestCreate_TemplateSource_NotFound(t *testing.T) {
	p, _ := newOwnershipSim(t)

	_, err := createFromTemplate(p, "orphan", "no-such-template")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to find template VM")
	assert.NotEqual(t, codes.InvalidArgument, status.Code(err), "a missing template keeps the retryable not-found path")
}

func TestCreate_TemplateSource_OverTheWire(t *testing.T) {
	p, _ := newOwnershipSim(t)
	c := serveOverGRPC(t, p)

	_, err := c.Create(context.Background(), contracts.CreateRequest{
		Name:  "steal",
		Image: contracts.VMImage{TemplateName: simUnstamped},
		Owner: ownerTeamA,
	})
	assert.True(t, contracts.IsInvalidSpec(err), "a regular VM as clone source must reach the manager as InvalidSpec, got %v", err)
}

// --- vcsim: ImagePrepare -------------------------------------------------------

// unreachableOVA would fail with a download error if ImagePrepare ever got past
// the idempotency gate, distinguishing "refused" from "tried to import".
const unreachableOVA = `{"source":{"vsphere":{"ovaURL":"http://127.0.0.1:1/never.ova"}}}`

func TestImagePrepare_OnlyTemplatesCountAsPrepared(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	t.Run("a same-named regular VM is not the prepared image", func(t *testing.T) {
		resp, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: unreachableOVA, TargetName: simUnstamped})
		requireCode(t, err, codes.InvalidArgument)
		assert.Nil(t, resp)
	})

	t.Run("an existing template is the prepared image", func(t *testing.T) {
		resp, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: unreachableOVA, TargetName: simTemplate})
		require.NoError(t, err)
		assert.Equal(t, simTemplate, resp.GetPreparedImageId())
	})

	t.Run("templateName source naming a regular VM is refused", func(t *testing.T) {
		resp, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
			ImageJson:  `{"source":{"vsphere":{"templateName":"` + simUnstamped + `"}}}`,
			TargetName: "prepared-from-vm",
		})
		requireCode(t, err, codes.InvalidArgument)
		assert.Nil(t, resp)
	})

	t.Run("ambiguous target template fails closed", func(t *testing.T) {
		seed := seededVMID(t, p, simTemplate)
		markAsTemplate(t, p, cloneVMInto(t, p, seed, defaultVMFolder(t, p), "img-dup"))
		markAsTemplate(t, p, cloneVMInto(t, p, seed, subFolder(t, p, "img-other"), "img-dup"))

		resp, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: unreachableOVA, TargetName: "img-dup"})
		requireCode(t, err, codes.InvalidArgument)
		assert.Nil(t, resp)
	})
}

// TestImagePrepare_RejectsUnsafeNamesBeforeAnyLookup: the provider's client and
// finder have no transport, so any vCenter call would fail; the unsafe target
// and template names must be refused first.
func TestImagePrepare_RejectsUnsafeNamesBeforeAnyLookup(t *testing.T) {
	empty := &vim25.Client{}
	p := &Provider{
		client: &govmomi.Client{Client: empty},
		finder: new(find.Finder), // zero Finder: any lookup dereferences a nil client
		config: &Config{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()

	for _, target := range []string{"vm-42", "a/b", "x:y"} {
		_, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: unreachableOVA, TargetName: target})
		requireCode(t, err, codes.InvalidArgument)
	}
	for _, tmpl := range []string{"vm-42", "VirtualMachine:vm-42", "/DC0/vm/../x"} {
		_, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
			ImageJson:  `{"source":{"vsphere":{"templateName":"` + tmpl + `"}}}`,
			TargetName: "safe-target",
		})
		requireCode(t, err, codes.InvalidArgument)
	}
}

// --- datacenter scoping of absolute template paths ------------------------------

func TestAbsoluteTemplatePathError(t *testing.T) {
	const vmFolder = "/DC0/vm"
	for _, ref := range []string{"/DC0/vm/tmpl", "/DC0/vm/templates/ubuntu"} {
		assert.NoError(t, absoluteTemplatePathError(ref, vmFolder), ref)
	}
	for _, ref := range []string{
		"/DC1/vm/tmpl",     // another datacenter
		"/DC01/vm/tmpl",    // a name prefix of the datacenter is not enough
		"/DC0/vmx/tmpl",    // a name prefix of the VM folder is not enough
		"/DC0/host/tmpl",   // another folder of the datacenter
		"/dc0/vm/tmpl",     // case variants fail closed
		"/DC0/vm",          // the folder itself is not under itself
		"/Other/DC0/vm/xx", // a same-named datacenter elsewhere
	} {
		requireCode(t, absoluteTemplatePathError(ref, vmFolder), codes.InvalidArgument)
	}
	// Without a known absolute VM-folder path nothing can be vouched for.
	requireCode(t, absoluteTemplatePathError("/DC0/vm/tmpl", ""), codes.InvalidArgument)
	requireCode(t, absoluteTemplatePathError("/DC0/vm/tmpl", "DC0/vm"), codes.InvalidArgument)
}

// TestTemplateSource_AbsolutePathInOtherDatacenterRejected: FindByInventoryPath
// reaches any datacenter the service account can see, but templateName is
// scoped to the Provider's default datacenter. A template in a second
// datacenter, reachable by absolute path, is refused before the lookup.
//
// The resolver is exercised directly: with two top-level datacenters the
// provider's own finder.DefaultDatacenter is ambiguous, so the finder is scoped
// to DC0 by hand exactly as the RPCs do after DefaultDatacenter.
func TestTemplateSource_AbsolutePathInOtherDatacenterRejected(t *testing.T) {
	model := simulator.VPX()
	model.Datacenter = 2
	require.NoError(t, model.Create())
	server := model.Service.NewServer()
	u := server.URL
	pw, _ := u.User.Password()
	cfg := &Config{
		Endpoint:           (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Username:           u.User.Username(),
		Password:           pw,
		InsecureSkipVerify: true,
	}
	client, finder, err := createVSphereClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = client.Logout(context.Background())
		server.Close()
		model.Remove()
	})
	ctx := context.Background()

	dc0, err := finder.Datacenter(ctx, "DC0")
	require.NoError(t, err)
	finder.SetDatacenter(dc0)
	p := &Provider{client: client, finder: finder, config: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Mark one VM in each datacenter as a template; both are reachable by path.
	for _, vmPath := range []string{"/DC0/vm/DC0_H0_VM0", "/DC1/vm/DC1_H0_VM0"} {
		obj, err := object.NewSearchIndex(client.Client).FindByInventoryPath(ctx, vmPath)
		require.NoError(t, err)
		require.NotNil(t, obj, "setup: %s is reachable by FindByInventoryPath", vmPath)
		markAsTemplate(t, p, obj.Reference().Value)
	}

	_, found, err := p.lookupTemplate(ctx, "/DC0/vm/DC0_H0_VM0")
	require.NoError(t, err)
	assert.True(t, found, "a template under the default datacenter's VM folder resolves")

	_, found, err = p.lookupTemplate(ctx, "/DC1/vm/DC1_H0_VM0")
	requireCode(t, err, codes.InvalidArgument)
	assert.False(t, found, "a template in another datacenter must not be reachable by absolute path")

	_, found, err = p.lookupTemplate(ctx, "DC1_H0_VM0")
	require.NoError(t, err)
	assert.False(t, found, "a bare name only searches the default datacenter")
}

// TestTemplateLookup_TransientErrorIsGeneric: a vCenter failure while reading
// candidates must not leak another object's identifier (MOID, fault text) into
// the caller's error, which ends up in tenant-visible status.
func TestTemplateLookup_TransientErrorIsGeneric(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	victim := seededVMID(t, p, simUnstamped)

	// Lose the session: every read now fails with NotAuthenticated.
	require.NoError(t, p.client.Logout(ctx))

	_, _, err := p.lookupTemplate(ctx, simUnstamped)
	require.Error(t, err)
	assert.NotEqual(t, codes.InvalidArgument, status.Code(err), "a transient failure stays retryable")
	assert.Contains(t, err.Error(), "details in the provider log")
	assert.NotContains(t, err.Error(), victim)
	assert.NotContains(t, strings.ToLower(err.Error()), "authenticat", "no vCenter fault text in the caller's error")
}
