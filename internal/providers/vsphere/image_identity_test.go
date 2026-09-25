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
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// vcsim tests of the ADR-0009 identity prepare (Slice 3).

const (
	// neverOVA is an OVA URL whose download fails fast (connection refused).
	// A prepare that answers without an error it would cause never downloaded.
	neverOVA = "http://127.0.0.1:1/never.ova"
	// legacyTotal is the ADR-0009 D7 deprecation counter.
	legacyTotal = "virtrigaud_provider_image_prepare_legacy_requests_total"
)

// --- fixtures ------------------------------------------------------------------

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newIdentitySim is newOwnershipSim (vcsim VPX, vCenter-faithful clone
// ExtraConfig) with the provider's logs captured and its DefaultFolder set.
func newIdentitySim(t *testing.T, defaultFolder string) (*Provider, *simulator.Model, *syncBuffer) {
	t.Helper()
	p, model := newOwnershipSim(t)
	logs := &syncBuffer{}
	p.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p.config.DefaultFolder = defaultFolder
	return p, model, logs
}

// testImage is VMImage team-a/ubuntu with uid.
func testImage(uid string) contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: uid, Namespace: "team-a", Name: "ubuntu"}
}

// ovaImageJSON is the VMImage spec JSON of an ovaURL source; extra is merged
// into the spec (e.g. a prepare timeout).
func ovaImageJSON(t *testing.T, ovaURL string, extra map[string]any) string {
	t.Helper()
	spec := map[string]any{"source": map[string]any{"vsphere": map[string]any{"ovaURL": ovaURL}}}
	for k, v := range extra {
		spec[k] = v
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	return string(b)
}

// identityReq is an ADR-0009 identity request for testImage(uid) and digest.
func identityReq(t *testing.T, ovaURL, uid, digest string) *providerv1.ImagePrepareRequest {
	t.Helper()
	img := testImage(uid)
	return &providerv1.ImagePrepareRequest{
		ImageJson:    ovaImageJSON(t, ovaURL, nil),
		Image:        &providerv1.ObjectIdentity{Uid: img.UID, Namespace: img.Namespace, Name: img.Name},
		SourceDigest: digest,
		Provider:     &providerv1.ObjectIdentity{Uid: testProviderUID, Namespace: "team-a", Name: "vsphere"},
	}
}

// artifactNameFor is the D1 vSphere artifact name of testImage(uid) and digest.
func artifactNameFor(t *testing.T, uid, digest string) string {
	t.Helper()
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleVSphere, testImage(uid), digest)
	require.NoError(t, err)
	return name
}

// stampConfig is the ExtraConfig stamp of testImage(uid) and digest, prepared at.
func stampConfig(uid, digest string, at time.Time) []types.BaseOptionValue {
	req := testIdentity(digest)
	req.Image.UID = uid
	return imageStampExtraConfig(imageartifact.NewStamp(req, at))
}

// folderAt resolves the folder at inventory path.
func folderAt(t *testing.T, p *Provider, inventoryPath string) *object.Folder {
	t.Helper()
	f, err := p.finder.Folder(context.Background(), inventoryPath)
	require.NoError(t, err)
	return f
}

// objectsNamed lists the VMs named name directly in folder.
func objectsNamed(t *testing.T, p *Provider, folder *object.Folder, name string) []types.ManagedObjectReference {
	t.Helper()
	refs, err := p.vmsNamedInFolder(context.Background(), folder, name)
	require.NoError(t, err)
	return refs
}

// plantVM creates a VM named name in folder, carrying extra ExtraConfig, as a
// template when template is set — an object VirtRigaud did not prepare (or a
// crashed or concurrent prepare's).
func plantVM(t *testing.T, p *Provider, folder *object.Folder, name string, extra []types.BaseOptionValue, template bool) types.ManagedObjectReference {
	t.Helper()
	ctx := context.Background()
	cluster, err := p.finder.ClusterComputeResource(ctx, simCluster)
	require.NoError(t, err)
	pool, err := cluster.ResourcePool(ctx)
	require.NoError(t, err)
	task, err := folder.CreateVM(ctx, types.VirtualMachineConfigSpec{
		Name:        name,
		GuestId:     "otherGuest",
		Files:       &types.VirtualMachineFileInfo{VmPathName: "[" + simDatastore + "]"},
		ExtraConfig: extra,
	}, pool, nil)
	require.NoError(t, err)
	info, err := task.WaitForResult(ctx, nil)
	require.NoError(t, err)
	ref, ok := info.Result.(types.ManagedObjectReference)
	require.True(t, ok)
	if template {
		require.NoError(t, object.NewVirtualMachine(p.client.Client, ref).MarkAsTemplate(ctx))
	}
	return ref
}

// objectSnapshot is what "untouched" compares: the object still exists with
// the same template flag and the same virtrigaud.* ExtraConfig.
type objectSnapshot struct {
	template bool
	reserved map[string]string
}

func snapshotObject(t *testing.T, p *Provider, ref types.ManagedObjectReference) objectSnapshot {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), ref,
		[]string{propConfigTemplate, propConfigExtraConfig}, &vm))
	require.NotNil(t, vm.Config)
	s := objectSnapshot{template: vm.Config.Template, reserved: map[string]string{}}
	for _, bov := range vm.Config.ExtraConfig {
		if o := bov.GetOptionValue(); o != nil && isReservedExtraConfigKey(o.Key) {
			v, _ := o.Value.(string)
			s.reserved[o.Key] = v
		}
	}
	return s
}

// stampOf reads and parses ref's image stamp.
func stampOf(t *testing.T, p *Provider, ref types.ManagedObjectReference) (*imageartifact.Stamp, error) {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), ref,
		[]string{propConfigExtraConfig}, &vm))
	require.NotNil(t, vm.Config)
	return imageStampFromExtraConfig(vm.Config.ExtraConfig)
}

// exists reports whether ref still exists.
func exists(t *testing.T, p *Provider, ref types.ManagedObjectReference) bool {
	t.Helper()
	var content []types.ObjectContent
	err := property.DefaultCollector(p.client.Client).Retrieve(context.Background(),
		[]types.ManagedObjectReference{ref}, []string{propName}, &content)
	return err == nil
}

// serveBody serves body with status at a URL ending in /image.ova.
func serveBody(t *testing.T, status int, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image.ova"
}

// tarOVA packs descriptor as the single member of an OVA.
func tarOVA(t *testing.T, descriptor string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "descriptor.ovf", Mode: 0o644, Size: int64(len(descriptor))}))
	_, err := tw.Write([]byte(descriptor))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// minimalOVAURL serves the diskless one-VM OVA (minimalDisklessOVF).
func minimalOVAURL(t *testing.T) string {
	t.Helper()
	ovaURL, _, closeSrv := newOVATarServer(t)
	t.Cleanup(closeSrv)
	return ovaURL
}

// duplicateNameMode selects how importFixture emulates vCenter's per-folder
// name uniqueness.
type duplicateNameMode int

const (
	// noDuplicateName leaves vcsim's behaviour (no uniqueness for ImportVApp).
	noDuplicateName duplicateNameMode = iota
	// duplicateNameViaLease reports DuplicateName through the HttpNfcLease's
	// error, as vCenter 8.0.2 does (verified in the lab, 2026-09-25):
	// ImportVApp returns a lease, and lease.Wait fails.
	duplicateNameViaLease
	// duplicateNameSync fails ImportVApp itself with a DuplicateName SOAP
	// fault (handled too, in case another vCenter version does).
	duplicateNameSync
)

// importFixture wraps the vim25 round-tripper around ImportVApp:
//   - beforeImport runs once, right before the first ImportVApp reaches vcsim
//     (a concurrent prepare creating its object first, with a lower MOID);
//   - duplicateName emulates vCenter's per-folder name uniqueness, which vcsim
//     does not model for ImportVApp (TestVcsimImportVAppAllowsDuplicateNames):
//     an ImportVApp whose entity name is taken in the target folder (by any
//     child, case-insensitively) fails with DuplicateName, through the lease
//     or synchronously;
//   - afterDuplicate runs after each emulated DuplicateName;
//   - afterComplete runs once, after the first successful HttpNfcLeaseComplete
//     (a concurrent prepare creating its object after ours, higher MOID).
type importFixture struct {
	soap.RoundTripper
	c              *vim25.Client
	duplicateName  duplicateNameMode
	beforeImport   func(folder types.ManagedObjectReference, name string)
	afterDuplicate func()
	afterComplete  func()
	beforeOnce     sync.Once
	afterOnce      sync.Once

	mu          sync.Mutex
	pendingDup  *types.DuplicateName // lease mode: the fault the next lease error becomes
	importVApps int
}

func (f *importFixture) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if r, ok := req.(*methods.ImportVAppBody); ok && r.Req != nil && r.Req.Folder != nil {
		f.mu.Lock()
		f.importVApps++
		f.mu.Unlock()
		if spec, ok := r.Req.Spec.(*types.VirtualMachineImportSpec); ok {
			name := spec.ConfigSpec.Name
			if f.beforeImport != nil {
				f.beforeOnce.Do(func() { f.beforeImport(*r.Req.Folder, name) })
			}
			if f.duplicateName != noDuplicateName {
				taken, err := f.childNamed(ctx, *r.Req.Folder, name)
				if err != nil {
					return err
				}
				if taken != nil {
					dup := &types.DuplicateName{Name: name, Object: *taken}
					if f.afterDuplicate != nil {
						defer f.afterDuplicate()
					}
					if f.duplicateName == duplicateNameSync {
						fault := &soap.Fault{Code: "ServerFaultCode", String: "The name '" + name + "' already exists."}
						fault.Detail.Fault = dup
						return soap.WrapSoapFault(fault)
					}
					// Lease mode: let vcsim fail the entity creation (an
					// empty name is an InvalidVmConfig), and turn the lease's
					// error into the DuplicateName vCenter reports.
					f.mu.Lock()
					f.pendingDup = dup
					f.mu.Unlock()
					spec.ConfigSpec.Name = ""
				}
			}
		}
	}
	err := f.RoundTripper.RoundTrip(ctx, req, res)
	if body, ok := res.(*methods.WaitForUpdatesExBody); ok && err == nil && body.Res != nil && body.Res.Returnval != nil {
		f.rewriteLeaseError(body.Res.Returnval)
	}
	if _, ok := req.(*methods.HttpNfcLeaseCompleteBody); ok && err == nil && f.afterComplete != nil {
		f.afterOnce.Do(f.afterComplete)
	}
	return err
}

// rewriteLeaseError replaces a pending lease error with the DuplicateName.
func (f *importFixture) rewriteLeaseError(set *types.UpdateSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingDup == nil {
		return
	}
	for i := range set.FilterSet {
		for j := range set.FilterSet[i].ObjectSet {
			obj := &set.FilterSet[i].ObjectSet[j]
			if obj.Obj.Type != "HttpNfcLease" {
				continue
			}
			for k := range obj.ChangeSet {
				if obj.ChangeSet[k].Name == "error" && obj.ChangeSet[k].Val != nil {
					obj.ChangeSet[k].Val = types.LocalizedMethodFault{
						Fault:            f.pendingDup,
						LocalizedMessage: "The name '" + f.pendingDup.Name + "' already exists.",
					}
					f.pendingDup = nil
					return
				}
			}
		}
	}
}

// imports is how many ImportVApp calls reached the fixture.
func (f *importFixture) imports() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.importVApps
}

func (f *importFixture) childNamed(ctx context.Context, folder types.ManagedObjectReference, name string) (*types.ManagedObjectReference, error) {
	var fo mo.Folder
	if err := property.DefaultCollector(f.c).RetrieveOne(ctx, folder, []string{"childEntity"}, &fo); err != nil {
		return nil, err
	}
	// Like vCenter: any child entity (VM, vApp, folder) holds its name, and
	// names compare case-insensitively.
	for _, child := range fo.ChildEntity {
		var content []types.ObjectContent
		if err := property.DefaultCollector(f.c).Retrieve(ctx, []types.ManagedObjectReference{child}, []string{propName}, &content); err != nil {
			return nil, err
		}
		for _, oc := range content {
			for _, prop := range oc.PropSet {
				if s, ok := prop.Val.(string); ok && prop.Name == propName && strings.EqualFold(s, name) {
					c := child
					return &c, nil
				}
			}
		}
	}
	return nil, nil
}

// installImportFixture wraps p's round-tripper with f.
func installImportFixture(p *Provider, f *importFixture) {
	f.RoundTripper = p.client.RoundTripper
	f.c = p.client.Client
	p.client.RoundTripper = f
}

// legacyCount reads the legacy-request counter for provider_type=vsphere.
func legacyCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != legacyTotal {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider_type" && l.GetValue() == vsphereProviderType {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// --- fresh prepare and reuse ------------------------------------------------------

func TestIdentityPrepare_FreshImportIsAStampedTemplateAtTheDerivedName(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	ctx := context.Background()
	name := artifactNameFor(t, testImageUID, testDigestA)
	require.True(t, strings.HasPrefix(name, "team-a.ubuntu_"), "D1: <namespace>.<name>_<h16>, got %q", name)

	resp, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)
	assert.Nil(t, resp.GetTask(), "a synchronous import returns no task")
	assert.Equal(t, "/DC0/vm/"+name, resp.GetPreparedImageId(), "prepared_image_id is the absolute inventory path")
	assert.Empty(t, resp.GetPreparedImagePath())

	a := resp.GetArtifact()
	require.NotNil(t, a, "identity responses echo the artifact")
	assert.Equal(t, name, a.GetName())
	assert.False(t, a.GetReused())
	assert.Equal(t, testImageUID, a.GetImage().GetUid())
	assert.Equal(t, "team-a", a.GetImage().GetNamespace())
	assert.Equal(t, "ubuntu", a.GetImage().GetName())
	assert.Equal(t, testDigestA, a.GetSourceDigest())

	refs := objectsNamed(t, p, defaultVMFolder(t, p), name)
	require.Len(t, refs, 1)
	assert.True(t, snapshotObject(t, p, refs[0]).template)
	stamp, err := stampOf(t, p, refs[0])
	require.NoError(t, err)
	require.NotNil(t, stamp)
	assert.True(t, stamp.Matches(testIdentity(testDigestA)))
	assert.Equal(t, imageartifact.StampIdentity{UID: testProviderUID, Namespace: "team-a", Name: "vsphere"}, stamp.PreparedBy)
	requireNoVMNamed(t, p, "ubuntu") // no bare-name artifact
}

func TestIdentityPrepare_ReusesTheMatchingTemplateWithoutImporting(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	ctx := context.Background()
	first, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)
	refs := objectsNamed(t, p, defaultVMFolder(t, p), first.GetArtifact().GetName())
	require.Len(t, refs, 1)

	// The digest identifies the source: an unreachable URL proves no download.
	again, err := p.ImagePrepare(ctx, identityReq(t, neverOVA, testImageUID, testDigestA))
	require.NoError(t, err)
	assert.True(t, again.GetArtifact().GetReused())
	assert.Equal(t, first.GetPreparedImageId(), again.GetPreparedImageId())
	assert.Equal(t, refs, objectsNamed(t, p, defaultVMFolder(t, p), first.GetArtifact().GetName()))
}

func TestIdentityPrepare_NewUIDOrDigestGetsANewArtifact(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	ctx := context.Background()
	ovaURL := minimalOVAURL(t)
	a, err := p.ImagePrepare(ctx, identityReq(t, ovaURL, testImageUID, testDigestA))
	require.NoError(t, err)
	b, err := p.ImagePrepare(ctx, identityReq(t, ovaURL, testImageUID, testDigestB))
	require.NoError(t, err)
	c, err := p.ImagePrepare(ctx, identityReq(t, ovaURL, testOtherUID, testDigestA))
	require.NoError(t, err)
	assert.NotEqual(t, a.GetPreparedImageId(), b.GetPreparedImageId(), "a changed spec.source is a new artifact")
	assert.NotEqual(t, a.GetPreparedImageId(), c.GetPreparedImageId(), "a re-created VMImage is a new artifact")
	for _, r := range []*providerv1.ImagePrepareResponse{a, b, c} {
		assert.False(t, r.GetArtifact().GetReused())
	}
}

// --- fail closed ------------------------------------------------------------------

func TestIdentityPrepare_ConflictLeavesTheObjectUntouched(t *testing.T) {
	now := time.Now()
	untrusted := append(stampConfig(testImageUID, testDigestA, now), optVal("VirtRigaud.Image.UID", testImageUID))
	for name, tc := range map[string]struct {
		extra    []types.BaseOptionValue
		template bool
	}{
		"unstamped template":                 {nil, true},
		"template of another VMImage":        {stampConfig(testOtherUID, testDigestA, now), true},
		"template of another source digest":  {stampConfig(testImageUID, testDigestB, now), true},
		"template with an untrusted stamp":   {untrusted, true},
		"unstamped VM":                       {nil, false},
		"unfinished VM of another VMImage":   {stampConfig(testOtherUID, testDigestA, now), false},
		"unfinished VM with untrusted stamp": {untrusted, false},
		// N1: stale, idle and powered off — would be "abandoned" if it were
		// this image's — yet another image's or source's: never destroyed.
		"stale unfinished VM of another VMImage":       {stampConfig(testOtherUID, testDigestA, now.Add(-3*time.Hour)), false},
		"stale unfinished VM of another source digest": {stampConfig(testImageUID, testDigestB, now.Add(-3*time.Hour)), false},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, logs := newIdentitySim(t, "")
			folder := defaultVMFolder(t, p)
			artifact := artifactNameFor(t, testImageUID, testDigestA)
			planted := plantVM(t, p, folder, artifact, tc.extra, tc.template)
			before := snapshotObject(t, p, planted)

			resp, err := p.ImagePrepare(context.Background(), identityReq(t, neverOVA, testImageUID, testDigestA))
			requireCode(t, err, codes.AlreadyExists)
			assert.Nil(t, resp)
			assert.NotContains(t, err.Error(), testOtherUID, "the message never names another image")
			assert.Contains(t, err.Error(), artifact)

			assert.Equal(t, []types.ManagedObjectReference{planted}, objectsNamed(t, p, folder, artifact),
				"nothing imported, nothing removed")
			assert.Equal(t, before, snapshotObject(t, p, planted), "never re-stamped or converted")
			assert.Contains(t, logs.String(), "refusing an object at the prepared-image artifact name")
		})
	}
}

// --- unfinished artifacts (D4 liveness) ----------------------------------------------

func TestIdentityPrepare_UnfinishedArtifact(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour)

	inProgress := map[string]func(t *testing.T, p *Provider, model *simulator.Model, ref types.ManagedObjectReference){
		"fresh": nil,
		"stale, but a task on it is running": func(t *testing.T, _ *Provider, model *simulator.Model, ref types.ManagedObjectReference) {
			reg := model.Map()
			tk := &simulator.Task{}
			tk.Info.State = types.TaskInfoStateRunning
			tk.Info.Entity = &ref
			tk.Info.DescriptionId = "ResourcePool.ImportVAppLRO"
			reg.Put(tk)
			vm, ok := reg.Get(ref).(*simulator.VirtualMachine)
			require.True(t, ok)
			reg.WithLock(model.Service.Context, vm, func() { vm.RecentTask = append(vm.RecentTask, tk.Self) })
		},
		"stale, but vCenter blocks its Destroy (defensive extra signal)": func(t *testing.T, _ *Provider, model *simulator.Model, ref types.ManagedObjectReference) {
			vm, ok := model.Map().Get(ref).(*simulator.VirtualMachine)
			require.True(t, ok)
			model.Map().WithLock(model.Service.Context, vm, func() { vm.DisabledMethod = []string{destroyTaskMethod} })
		},
		"stale, and the state of its task cannot be read (fail closed)": func(t *testing.T, p *Provider, model *simulator.Model, ref types.ManagedObjectReference) {
			reg := model.Map()
			tk := &simulator.Task{}
			tk.Info.State = types.TaskInfoStateRunning
			tk.Info.Entity = &ref
			reg.Put(tk)
			vm, ok := reg.Get(ref).(*simulator.VirtualMachine)
			require.True(t, ok)
			reg.WithLock(model.Service.Context, vm, func() { vm.RecentTask = append(vm.RecentTask, tk.Self) })
			p.client.RoundTripper = &failTaskReads{RoundTripper: p.client.RoundTripper}
		},
	}
	for name, setup := range inProgress {
		t.Run("in progress: "+name, func(t *testing.T) {
			p, model, _ := newIdentitySim(t, "")
			folder := defaultVMFolder(t, p)
			artifact := artifactNameFor(t, testImageUID, testDigestA)
			at := time.Now()
			if setup != nil {
				at = stale
			}
			planted := plantVM(t, p, folder, artifact, stampConfig(testImageUID, testDigestA, at), false)
			if setup != nil {
				setup(t, p, model, planted)
			}
			before := snapshotObject(t, p, planted)

			_, err := p.ImagePrepare(context.Background(), identityReq(t, neverOVA, testImageUID, testDigestA))
			requireCode(t, err, codes.Unavailable)
			assert.Contains(t, err.Error(), "still being prepared")
			assert.Equal(t, []types.ManagedObjectReference{planted}, objectsNamed(t, p, folder, artifact))
			assert.Equal(t, before, snapshotObject(t, p, planted), "a live object is never touched")
		})
	}

	t.Run("in progress: the staleness bound follows spec.prepare.timeout", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		planted := plantVM(t, p, folder, artifact, stampConfig(testImageUID, testDigestA, stale), false)

		req := identityReq(t, neverOVA, testImageUID, testDigestA)
		req.ImageJson = ovaImageJSON(t, neverOVA, map[string]any{"prepare": map[string]any{"timeout": "2h"}})
		_, err := p.ImagePrepare(context.Background(), req)
		requireCode(t, err, codes.Unavailable) // bound 4h > 3h old
		assert.True(t, exists(t, p, planted))
	})

	t.Run("conflict: stale but powered on", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		planted := plantVM(t, p, folder, artifact, stampConfig(testImageUID, testDigestA, stale), false)
		task, err := object.NewVirtualMachine(p.client.Client, planted).PowerOn(context.Background())
		require.NoError(t, err)
		require.NoError(t, task.Wait(context.Background()))

		_, err = p.ImagePrepare(context.Background(), identityReq(t, neverOVA, testImageUID, testDigestA))
		requireCode(t, err, codes.AlreadyExists)
		assert.True(t, exists(t, p, planted), "a powered-on VM is never destroyed")
	})

	t.Run("abandoned: removed, then imported again", func(t *testing.T) {
		p, _, logs := newIdentitySim(t, "")
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		planted := plantVM(t, p, folder, artifact, stampConfig(testImageUID, testDigestA, stale), false)

		resp, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		require.NoError(t, err)
		assert.False(t, resp.GetArtifact().GetReused())
		assert.False(t, exists(t, p, planted), "the abandoned object of this image was removed")
		refs := objectsNamed(t, p, folder, artifact)
		require.Len(t, refs, 1)
		assert.NotEqual(t, planted, refs[0])
		assert.True(t, snapshotObject(t, p, refs[0]).template)
		assert.Contains(t, logs.String(), "removing an abandoned")
	})
}

// --- import folder (D5) ------------------------------------------------------------

func TestIdentityPrepare_ImportFolderResolution(t *testing.T) {
	t.Run("configured folder: probe and import are scoped to it", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "images")
		images := subFolder(t, p, "images")
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		// A matching template elsewhere in the datacenter is neither reused nor
		// a conflict: the uniqueness scope is the import folder.
		elsewhere := plantVM(t, p, defaultVMFolder(t, p), artifact, stampConfig(testImageUID, testDigestA, time.Now()), true)

		resp, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		require.NoError(t, err)
		assert.Equal(t, "/DC0/vm/images/"+artifact, resp.GetPreparedImageId())
		assert.False(t, resp.GetArtifact().GetReused())
		assert.Len(t, objectsNamed(t, p, images, artifact), 1)
		assert.True(t, exists(t, p, elsewhere))
	})

	for name, tc := range map[string]struct {
		folder string
		setup  func(t *testing.T, p *Provider)
		reason string
	}{
		"missing":               {folder: "no-such-folder", reason: "does not exist"},
		"ambiguous":             {folder: "team-*", setup: func(t *testing.T, p *Provider) { subFolder(t, p, "team-x"); subFolder(t, p, "team-y") }, reason: "more than one"},
		"outside the VM folder": {folder: "/DC0/datastore", reason: "not under"},
	} {
		t.Run("configured but unusable, never a fallback: "+name, func(t *testing.T) {
			p, _, _ := newIdentitySim(t, tc.folder)
			if tc.setup != nil {
				tc.setup(t, p)
			}
			_, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
			// A configuration problem: retried by the manager, but not an
			// Unavailable that would count toward its circuit breaker.
			requireCode(t, err, codes.FailedPrecondition)
			assert.Contains(t, err.Error(), tc.reason)
			requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))
		})
	}
}

// TestImportFolderLookupReason: only a definitive "missing" or "ambiguous"
// finder answer is a configuration problem (FailedPrecondition); any other
// lookup error is a vCenter failure (Unavailable). Neither ever falls back.
func TestImportFolderLookupReason(t *testing.T) {
	assert.Equal(t, "it does not exist", importFolderLookupReason(&find.NotFoundError{}))
	assert.Equal(t, "it does not exist", importFolderLookupReason(fmt.Errorf("x: %w", &find.NotFoundError{})))
	assert.Equal(t, "it names more than one folder", importFolderLookupReason(&find.MultipleFoundError{}))
	assert.Empty(t, importFolderLookupReason(soap.WrapVimFault(&types.NotAuthenticated{})))
	assert.Empty(t, importFolderLookupReason(fmt.Errorf("connection reset by peer")))
}

// --- OVF shape and failure classification -------------------------------------------

// multiVMOVF is an OVF whose content is a VirtualSystemCollection (a vApp).
const multiVMOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">
  <References/>
  <VirtualSystemCollection ovf:id="vapp">
    <Info>A vApp</Info>
    <VirtualSystem ovf:id="vm1"><Info>one</Info></VirtualSystem>
    <VirtualSystem ovf:id="vm2"><Info>two</Info></VirtualSystem>
  </VirtualSystemCollection>
</Envelope>`

// vAppImportSpec wraps the round-tripper so CreateImportSpec returns a
// VirtualAppImportSpec around the VM spec, as vCenter does for a multi-VM OVF
// (vcsim only produces VirtualMachineImportSpec).
type vAppImportSpec struct{ soap.RoundTripper }

func (r *vAppImportSpec) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	if body, ok := res.(*methods.CreateImportSpecBody); ok && body.Res != nil {
		body.Res.Returnval.ImportSpec = &types.VirtualAppImportSpec{
			Name:  "vapp",
			Child: []types.BaseImportSpec{body.Res.Returnval.ImportSpec},
		}
	}
	return nil
}

func TestIdentityPrepare_MultiVMOVFIsInvalidSpec(t *testing.T) {
	t.Run("VirtualSystemCollection", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		_, err := p.ImagePrepare(context.Background(),
			identityReq(t, serveBody(t, http.StatusOK, tarOVA(t, multiVMOVF)), testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "more than one virtual machine")
		requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))
	})
	t.Run("vApp import spec", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		p.client.RoundTripper = &vAppImportSpec{RoundTripper: p.client.RoundTripper}
		_, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))
	})
}

func TestIdentityPrepare_FailureClassification(t *testing.T) {
	validOVA := tarOVA(t, minimalDisklessOVF)
	permanent := map[string]string{
		"source 404":                  serveBody(t, http.StatusNotFound, nil),
		"source 410":                  serveBody(t, http.StatusGone, nil),
		"source 403":                  serveBody(t, http.StatusForbidden, nil),
		"not a tar archive":           serveBody(t, http.StatusOK, bytes.Repeat([]byte{0x5a}, 2048)),
		"tar without a descriptor":    serveBody(t, http.StatusOK, tarOVAMember(t, "disk.vmdk", "x")),
		"descriptor is not valid OVF": serveBody(t, http.StatusOK, tarOVA(t, "<Envelope><not-closed>")),
		"not an http(s) URL":          "ftp://127.0.0.1/image.ova",
	}
	transient := map[string]string{
		"source 500":         serveBody(t, http.StatusInternalServerError, nil),
		"source 503":         serveBody(t, http.StatusServiceUnavailable, nil),
		"source 429":         serveBody(t, http.StatusTooManyRequests, nil),
		"source 408":         serveBody(t, http.StatusRequestTimeout, nil),
		"source unreachable": neverOVA,
	}
	p, _, _ := newIdentitySim(t, "")
	for name, u := range permanent {
		t.Run("permanent: "+name, func(t *testing.T) {
			_, err := p.ImagePrepare(context.Background(), identityReq(t, u, testImageUID, testDigestA))
			requireCode(t, err, codes.InvalidArgument)
		})
	}
	for name, u := range transient {
		t.Run("transient: "+name, func(t *testing.T) {
			_, err := p.ImagePrepare(context.Background(), identityReq(t, u, testImageUID, testDigestA))
			requireCode(t, err, codes.Unavailable)
			assert.Equal(t, []string{contracts.ImageSourceUnavailableReason}, errorReasons(err),
				"the source's fault: retried, but kept out of the manager's circuit breaker")
		})
	}
	t.Run("permanent: checksum mismatch", func(t *testing.T) {
		req := identityReq(t, neverOVA, testImageUID, testDigestA)
		req.ImageJson = `{"source":{"vsphere":{"ovaURL":"` + serveBody(t, http.StatusOK, validOVA) +
			`","checksum":"` + strings.Repeat("0", 64) + `","checksumType":"sha256"}}}`
		_, err := p.ImagePrepare(context.Background(), req)
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "checksum mismatch")
	})
	requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))

	t.Run("credentials and tokens in the URL never reach the error or the log", func(t *testing.T) {
		p, _, logs := newIdentitySim(t, "")
		u := strings.Replace(serveBody(t, http.StatusNotFound, nil), "http://", "http://user:s3cret@", 1) + "?token=t0ken"
		_, err := p.ImagePrepare(context.Background(), identityReq(t, u, testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		for _, secret := range []string{"s3cret", "t0ken"} {
			assert.NotContains(t, err.Error(), secret)
			assert.NotContains(t, logs.String(), secret)
		}
	})

	t.Run("transient: vCenter session lost", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		require.NoError(t, p.client.Logout(context.Background()))
		_, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable)
		assert.Contains(t, err.Error(), "details in the provider log")
		assert.Empty(t, errorReasons(err), "vCenter itself failing counts toward the circuit breaker")
	})

	t.Run("parser errors never quote the downloaded content", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		for name, body := range map[string][]byte{
			"an HTML page as the descriptor": tarOVA(t, "<html><head><title>internal-admin-console</title></head></html>"),
			"not a tar archive":              []byte("<html><title>internal-admin-console</title></html>" + strings.Repeat(" ", 600)),
		} {
			_, err := p.ImagePrepare(context.Background(),
				identityReq(t, serveBody(t, http.StatusOK, body), testImageUID, testDigestA))
			requireCode(t, err, codes.InvalidArgument)
			for _, leak := range []string{"html", "internal-admin-console", "archive/tar", "XML", "xml"} {
				assert.NotContains(t, err.Error(), leak, name)
			}
		}
	})

	t.Run("an import vCenter refuses for the image's content is the source's fault", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		p.client.RoundTripper = &failImportVApp{RoundTripper: p.client.RoundTripper,
			fault: &types.VmConfigFault{}}
		_, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable)
		assert.Equal(t, []string{contracts.ImageSourceUnavailableReason}, errorReasons(err))
		assert.NotContains(t, err.Error(), "VmConfigFault", "vCenter fault text stays in the provider log")
	})

	t.Run("an import that loses the vCenter session is vCenter's fault", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		p.client.RoundTripper = &failImportVApp{RoundTripper: p.client.RoundTripper,
			fault: &types.NotAuthenticated{}}
		_, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable)
		assert.Empty(t, errorReasons(err))
	})
}

// failTaskReads fails every property read of a Task with a transient fault.
type failTaskReads struct{ soap.RoundTripper }

func (r *failTaskReads) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if b, ok := req.(*methods.RetrievePropertiesBody); ok && b.Req != nil {
		for _, spec := range b.Req.SpecSet {
			for _, o := range spec.ObjectSet {
				if o.Obj.Type == "Task" {
					return soap.WrapVimFault(&types.HostCommunication{})
				}
			}
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// TestDestroyOwnImport_AlreadyDeletedIsSuccess: vCenter deletes the entity of
// an aborted import lease (verified on vCenter 8.0.2), so this call's cleanup
// finds it gone — that is success, not a failure to log or retry.
func TestDestroyOwnImport_AlreadyDeletedIsSuccess(t *testing.T) {
	p, _, logs := newIdentitySim(t, "")
	gone := vmRef("vm-987654")
	p.destroyOwnImport(context.Background(), gone, "team-a.ubuntu_0000000000000000")
	assert.Contains(t, logs.String(), "already gone")
	assert.NotContains(t, logs.String(), "could not destroy")
	require.NoError(t, p.destroyVM(context.Background(), gone))
	present, err := p.destroyVMIfPresent(context.Background(), gone)
	require.NoError(t, err)
	assert.False(t, present)
}

// failImportVApp answers every ImportVApp with fault, as vCenter would.
type failImportVApp struct {
	soap.RoundTripper
	fault types.BaseMethodFault
}

func (r *failImportVApp) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if _, ok := req.(*methods.ImportVAppBody); ok {
		return soap.WrapVimFault(r.fault)
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// errorReasons returns the google.rpc.ErrorInfo reasons (VirtRigaud's domain)
// a gRPC status error carries.
func errorReasons(err error) []string {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	var reasons []string
	for _, d := range st.Details() {
		if info, isInfo := d.(*errdetails.ErrorInfo); isInfo && info.GetDomain() == contracts.ErrorInfoDomain {
			reasons = append(reasons, info.GetReason())
		}
	}
	return reasons
}

func TestIsVCenterUnreachable(t *testing.T) {
	for name, err := range map[string]error{
		"session expired":  soap.WrapVimFault(&types.NotAuthenticated{}),
		"no rights":        fmt.Errorf("ImportVApp: %w", soap.WrapVimFault(&types.NoPermission{})),
		"host unreachable": &task.Error{LocalizedMethodFault: &types.LocalizedMethodFault{Fault: &types.HostCommunication{}}},
		"transport":        &url.Error{Op: "Post", URL: "https://vc/sdk", Err: fmt.Errorf("connection refused")},
		"deadline":         fmt.Errorf("wait: %w", context.DeadlineExceeded),
	} {
		assert.True(t, isVCenterUnreachable(err), name)
	}
	for name, err := range map[string]error{
		"config fault in the lease": &task.Error{LocalizedMethodFault: &types.LocalizedMethodFault{Fault: &types.VmConfigFault{}}},
		"NFC upload refused":        fmt.Errorf("upload disk1.vmdk: %w", fmt.Errorf("400 Bad Request")),
		"nil":                       nil,
	} {
		assert.False(t, isVCenterUnreachable(err), name)
	}
}

// TestIsNotFound (review N1): only a ManagedObjectNotFound naming ref itself
// proves ref is gone; one for the destroy task or a parent does not.
func TestIsNotFound(t *testing.T) {
	vm := types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-42"}
	other := types.ManagedObjectReference{Type: "Task", Value: "task-7"}
	notFound := func(obj types.ManagedObjectReference) *types.ManagedObjectNotFound {
		return &types.ManagedObjectNotFound{Obj: obj}
	}

	for name, err := range map[string]error{
		"soap fault":  fmt.Errorf("Destroy: %w", soap.WrapVimFault(notFound(vm))),
		"task result": &task.Error{LocalizedMethodFault: &types.LocalizedMethodFault{Fault: notFound(vm)}},
	} {
		assert.True(t, isNotFound(err, vm), name)
	}
	for name, err := range map[string]error{
		"another object":        soap.WrapVimFault(notFound(other)),
		"same MOID, other type": soap.WrapVimFault(notFound(types.ManagedObjectReference{Type: "Folder", Value: "vm-42"})),
		"another fault":         soap.WrapVimFault(&types.NoPermission{}),
		"nil":                   nil,
	} {
		assert.False(t, isNotFound(err, vm), name)
	}
}

// tarOVAMember packs one member name with content.
func tarOVAMember(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// --- the OVF cannot forge a stamp (D3) --------------------------------------------------

func TestIdentityPrepare_OVFSuppliedImageStampIsStripped(t *testing.T) {
	p, _, logs := newIdentitySim(t, "")
	p.client.RoundTripper = &ovfExtraConfigInjector{
		RoundTripper: p.client.RoundTripper,
		extra: []types.BaseOptionValue{
			optVal(imageStampKeyUID, testOtherUID),
			optVal("VirtRigaud.Image.SourceDigest", testDigestB),
			optVal(imageStampKeyVersion, "1"),
			optVal("guestinfo.keep", "yes"),
		},
	}
	resp, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)

	refs := objectsNamed(t, p, defaultVMFolder(t, p), resp.GetArtifact().GetName())
	require.Len(t, refs, 1)
	stamp, err := stampOf(t, p, refs[0])
	require.NoError(t, err, "no OVF-supplied key survives next to the real stamp")
	require.NotNil(t, stamp)
	assert.True(t, stamp.Matches(testIdentity(testDigestA)), "the stamp is the requester's, not the OVF's")
	assert.Equal(t, "yes", extraConfigOf(t, p, refs[0])["guestinfo.keep"])
	assert.Contains(t, logs.String(), "removed VirtRigaud-reserved ExtraConfig keys")
}

// --- DuplicateName and convergence (D6) -------------------------------------------------

// TestVcsimImportVAppAllowsDuplicateNames pins the simulator behaviour the
// ADR-0009 D6 tests work around: vcsim (govmomi v0.52.0) does NOT model
// vCenter's per-folder name uniqueness for ResourcePool.ImportVApp (its
// createVM task never checks the folder), so a second same-named import
// succeeds. If this starts failing, vcsim models DuplicateName and the
// importFixture emulation can be dropped. Real vCenter must be checked in the
// lab (the Slice 3 merge gate).
func TestVcsimImportVAppAllowsDuplicateNames(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	ctx := context.Background()
	folder := defaultVMFolder(t, p)
	plantVM(t, p, folder, "dup-import", nil, false)

	ovaURL := minimalOVAURL(t)
	localPath, cleanup, err := p.downloadOVA(ctx, ovaURL)
	require.NoError(t, err)
	defer cleanup()
	archive, descriptor, err := p.newOVAArchive(localPath, ovaURL)
	require.NoError(t, err)
	pool, ds, err := p.resolveImageComputeAndStorage(ctx, p.finder, "")
	require.NoError(t, err)
	name := "dup-import"
	_, err = (&importer.Importer{Log: p.ovaImportLog, Client: p.client.Client, Finder: p.finder,
		Datastore: ds, ResourcePool: pool, Folder: folder, Archive: archive}).
		Import(ctx, descriptor, importer.Options{Name: &name})
	require.NoError(t, err, "vcsim accepts a same-named ImportVApp (vCenter answers DuplicateName)")
	assert.Len(t, objectsNamed(t, p, folder, "dup-import"), 2)
}

func TestIdentityPrepare_DuplicateNameReprobes(t *testing.T) {
	for name, tc := range map[string]struct {
		extra    func() []types.BaseOptionValue
		template bool
		wantCode codes.Code // codes.OK: reused
	}{
		"a matching template appeared: reuse": {
			extra: func() []types.BaseOptionValue { return stampConfig(testImageUID, testDigestA, time.Now()) }, template: true,
		},
		"a matching unfinished import appeared: in progress": {
			extra: func() []types.BaseOptionValue { return stampConfig(testImageUID, testDigestA, time.Now()) }, wantCode: codes.Unavailable,
		},
		"a foreign object appeared: conflict": {
			extra: func() []types.BaseOptionValue { return stampConfig(testOtherUID, testDigestA, time.Now()) }, template: true, wantCode: codes.AlreadyExists,
		},
	} {
		for modeName, mode := range map[string]duplicateNameMode{
			"through the lease (vCenter 8.0.2)": duplicateNameViaLease,
			"from ImportVApp":                   duplicateNameSync,
		} {
			t.Run(name+", "+modeName, func(t *testing.T) {
				p, _, logs := newIdentitySim(t, "")
				folder := defaultVMFolder(t, p)
				artifact := artifactNameFor(t, testImageUID, testDigestA)
				var planted types.ManagedObjectReference
				installImportFixture(p, &importFixture{
					duplicateName: mode,
					// Another prepare wins the race between our probe and our import.
					beforeImport: func(_ types.ManagedObjectReference, n string) {
						planted = plantVM(t, p, folder, n, tc.extra(), tc.template)
					},
				})

				resp, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
				if tc.wantCode == codes.OK {
					require.NoError(t, err)
					assert.True(t, resp.GetArtifact().GetReused())
					assert.Equal(t, "/DC0/vm/"+artifact, resp.GetPreparedImageId())
				} else {
					requireCode(t, err, tc.wantCode)
				}
				assert.Equal(t, []types.ManagedObjectReference{planted}, objectsNamed(t, p, folder, artifact),
					"exactly the winner's object remains")
				assert.Contains(t, logs.String(), "DuplicateName")
			})
		}
	}
}

// TestIdentityPrepare_DuplicateNameHeldOutOfSight: vCenter refuses the name
// for an object the folder probe cannot see (a folder or vApp with the name, or
// a VM whose name differs only in case). The prepare is a Conflict at once — it
// does not import, and re-download, again and again — and the holder is left
// alone. A holder that disappears meanwhile (a concurrent prepare destroying
// its own) is not a conflict: the import is retried.
func TestIdentityPrepare_DuplicateNameHeldOutOfSight(t *testing.T) {
	artifact := artifactNameFor(t, testImageUID, testDigestA)
	for name, plant := range map[string]func(t *testing.T, p *Provider) types.ManagedObjectReference{
		"a folder with the name": func(t *testing.T, p *Provider) types.ManagedObjectReference {
			return subFolder(t, p, artifact).Reference()
		},
		"a VM whose name differs in case": func(t *testing.T, p *Provider) types.ManagedObjectReference {
			return plantVM(t, p, defaultVMFolder(t, p), strings.ToUpper(artifact),
				stampConfig(testImageUID, testDigestA, time.Now()), true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, logs := newIdentitySim(t, "")
			holder := plant(t, p)
			fx := &importFixture{duplicateName: duplicateNameViaLease}
			installImportFixture(p, fx)
			u, hits := countingServer(t, ".ova", tarOVA(t, minimalDisklessOVF))

			_, err := p.ImagePrepare(context.Background(), identityReq(t, u, testImageUID, testDigestA))
			requireCode(t, err, codes.AlreadyExists)
			assert.Equal(t, int32(1), hits.Load(), "downloaded once")
			assert.Equal(t, 1, fx.imports(), "imported once: no loop")
			assert.True(t, exists(t, p, holder), "the holder is left alone")
			assert.Contains(t, logs.String(), "held by an object that is not a VM with exactly that name")
			assert.Empty(t, objectsNamed(t, p, defaultVMFolder(t, p), artifact))
		})
	}

	t.Run("a holder that vanished is retried, not a conflict", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		folder := defaultVMFolder(t, p)
		var peer types.ManagedObjectReference
		fx := &importFixture{duplicateName: duplicateNameViaLease}
		fx.beforeImport = func(_ types.ManagedObjectReference, n string) {
			peer = plantVM(t, p, folder, n, stampConfig(testImageUID, testDigestA, time.Now()), false)
		}
		// The peer converges and destroys its own object right after our
		// DuplicateName, before we probe again.
		fx.afterDuplicate = func() { require.NoError(t, p.destroyVM(context.Background(), peer)) }
		installImportFixture(p, fx)

		resp, err := p.ImagePrepare(context.Background(), identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		require.NoError(t, err)
		assert.False(t, resp.GetArtifact().GetReused())
		assert.Equal(t, 2, fx.imports())
		refs := objectsNamed(t, p, folder, artifact)
		require.Len(t, refs, 1)
		assert.NotEqual(t, peer, refs[0])
	})
}

func TestIdentityPrepare_LowestMOIDConvergence(t *testing.T) {
	t.Run("a concurrent object with a lower MOID wins; ours is destroyed", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		ctx := context.Background()
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		var peer types.ManagedObjectReference
		installImportFixture(p, &importFixture{beforeImport: func(_ types.ManagedObjectReference, n string) {
			peer = plantVM(t, p, folder, n, stampConfig(testImageUID, testDigestA, time.Now()), false)
		}})

		_, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable) // the survivor is still being prepared
		assert.Equal(t, []types.ManagedObjectReference{peer}, objectsNamed(t, p, folder, artifact),
			"the call whose object is not the lowest MOID destroyed its own")

		// The peer finishes; the next call reuses it.
		require.NoError(t, object.NewVirtualMachine(p.client.Client, peer).MarkAsTemplate(ctx))
		resp, err := p.ImagePrepare(ctx, identityReq(t, neverOVA, testImageUID, testDigestA))
		require.NoError(t, err)
		assert.True(t, resp.GetArtifact().GetReused())
	})

	t.Run("a concurrent object with a higher MOID loses; ours is kept", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		ctx := context.Background()
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		var peer types.ManagedObjectReference
		installImportFixture(p, &importFixture{afterComplete: func() {
			peer = plantVM(t, p, folder, artifact, stampConfig(testImageUID, testDigestA, time.Now()), false)
		}})

		_, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable) // not handed out while the name is ambiguous
		refs := objectsNamed(t, p, folder, artifact)
		require.Len(t, refs, 2)
		var ours types.ManagedObjectReference
		for _, r := range refs {
			if r != peer {
				ours = r
			}
		}
		assert.True(t, moidLess(ours.Value, peer.Value))
		assert.True(t, snapshotObject(t, p, ours).template, "the lowest-MOID object is completed")

		// The peer converges (destroys its own); the next call reuses ours.
		require.NoError(t, p.destroyVM(ctx, peer))
		resp, err := p.ImagePrepare(ctx, identityReq(t, neverOVA, testImageUID, testDigestA))
		require.NoError(t, err)
		assert.True(t, resp.GetArtifact().GetReused())
		assert.Equal(t, []types.ManagedObjectReference{ours}, objectsNamed(t, p, folder, artifact))
	})

	t.Run("concurrent prepares converge on one template", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		ctx := context.Background()
		folder := defaultVMFolder(t, p)
		artifact := artifactNameFor(t, testImageUID, testDigestA)
		ovaURL := minimalOVAURL(t)

		const n = 4
		errs := make([]error, n)
		reqs := make([]*providerv1.ImagePrepareRequest, n)
		for i := range reqs {
			reqs[i] = identityReq(t, ovaURL, testImageUID, testDigestA)
		}
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = p.ImagePrepare(ctx, reqs[i])
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				assert.Equal(t, codes.Unavailable, status.Code(err), "only success or a retryable error: %v", err)
			}
		}

		// Retries settle on exactly one template.
		var resp *providerv1.ImagePrepareResponse
		require.Eventually(t, func() bool {
			var err error
			resp, err = p.ImagePrepare(ctx, identityReq(t, ovaURL, testImageUID, testDigestA))
			return err == nil
		}, 30*time.Second, 50*time.Millisecond)
		refs := objectsNamed(t, p, folder, artifact)
		require.Len(t, refs, 1)
		assert.True(t, snapshotObject(t, p, refs[0]).template)
		assert.Equal(t, "/DC0/vm/"+artifact, resp.GetPreparedImageId())
	})
}

// --- consumers of the artifact -----------------------------------------------------------

func TestCreateAndCloneClearTheImageStamp(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	ctx := context.Background()
	prepared, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)
	tmplRefs := objectsNamed(t, p, defaultVMFolder(t, p), prepared.GetArtifact().GetName())
	require.Len(t, tmplRefs, 1)

	// Control: vCenter copies ExtraConfig to a clone, so a plain clone carries
	// the image stamp; the assertions below pass only because Create and Clone
	// clear it.
	plain := cloneVMInto(t, p, tmplRefs[0].Value, defaultVMFolder(t, p), "plain-copy")
	inherited, err := stampOf(t, p, vmRef(plain))
	require.NoError(t, err)
	require.NotNil(t, inherited, "control: a plain clone inherits the template's image stamp")

	created, err := p.Create(ctx, &providerv1.CreateRequest{
		Name:      "web",
		ImageJson: `{"TemplateName":"` + prepared.GetPreparedImageId() + `"}`,
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)
	stamp, err := stampOf(t, p, vmRef(created.Id))
	require.NoError(t, err)
	assert.Nil(t, stamp, "a VM created from a prepared template carries no image stamp")

	cloned, err := p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: plain, TargetName: "web-copy"})
	require.NoError(t, err)
	stamp, err = stampOf(t, p, vmRef(cloned.TargetVmId))
	require.NoError(t, err)
	assert.Nil(t, stamp, "a clone carries no image stamp")
}

func TestCreate_ClonesTheVerifiedArtifactByInventoryPath(t *testing.T) {
	p, _, _ := newIdentitySim(t, "images")
	ctx := context.Background()
	subFolder(t, p, "images")
	prepared, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)
	artifact := prepared.GetArtifact().GetName()
	ours := objectsNamed(t, p, folderAt(t, p, "/DC0/vm/images"), artifact)
	require.Len(t, ours, 1)

	// A same-named template elsewhere in the datacenter, with a different
	// shape: a bare-name lookup would be ambiguous (or pick it).
	decoy := cloneVMInto(t, p, seededVMID(t, p, simTemplate), defaultVMFolder(t, p), artifact)
	setNumCPU(t, p, decoy, 4)
	markAsTemplate(t, p, decoy)
	require.NotEqual(t, numCPU(t, p, ours[0].Value), numCPU(t, p, decoy))

	_, err = createFromTemplate(p, "by-bare-name", artifact)
	requireCode(t, err, codes.InvalidArgument) // ambiguous by name

	vm, err := createFromTemplate(p, "by-prepared-id", prepared.GetPreparedImageId())
	require.NoError(t, err)
	assert.Equal(t, numCPU(t, p, ours[0].Value), numCPU(t, p, vm.Id), "Create cloned the verified artifact")
}

// --- legacy mode (D7) -------------------------------------------------------------------

func TestImagePrepare_LegacyRequestTakesThePreADRPath(t *testing.T) {
	p, _, logs := newIdentitySim(t, "")
	ctx := context.Background()
	before := legacyCount(t)
	legacy := &providerv1.ImagePrepareRequest{ImageJson: ovaImageJSON(t, minimalOVAURL(t), nil), TargetName: "legacy-img"}

	resp, err := p.ImagePrepare(ctx, legacy)
	require.NoError(t, err)
	assert.Equal(t, "legacy-img", resp.GetPreparedImageId(), "the bare name, the shape an older manager expects")
	assert.Nil(t, resp.GetArtifact(), "no artifact echo in legacy mode")
	refs := objectsNamed(t, p, defaultVMFolder(t, p), "legacy-img")
	require.Len(t, refs, 1)
	stamp, err := stampOf(t, p, refs[0])
	require.NoError(t, err)
	assert.Nil(t, stamp, "a legacy artifact is unstamped")

	resp, err = p.ImagePrepare(ctx, legacy)
	require.NoError(t, err)
	assert.Equal(t, "legacy-img", resp.GetPreparedImageId(), "reused by bare name")

	assert.Equal(t, before+2, legacyCount(t), "one counter increment per legacy request")
	assert.Equal(t, 2, strings.Count(logs.String(), imageartifact.LegacyRequestWarning))
	assert.Contains(t, logs.String(), "level=WARN")

	t.Run("legacy and identity artifacts are disjoint", func(t *testing.T) {
		id, err := p.ImagePrepare(ctx, identityReq(t, minimalOVAURL(t), testImageUID, testDigestA))
		require.NoError(t, err)
		assert.False(t, id.GetArtifact().GetReused(), "the legacy template is not adopted")
		assert.Equal(t, refs, objectsNamed(t, p, defaultVMFolder(t, p), "legacy-img"), "and left alone")

		n := legacyCount(t)
		_, err = p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{ImageJson: legacy.ImageJson, TargetName: id.GetArtifact().GetName()})
		requireCode(t, err, codes.InvalidArgument) // a bare name can never be an artifact name ('_')
		assert.Equal(t, n, legacyCount(t), "a malformed request is refused before it is served")
	})
}

func TestGetCapabilities_ImageArtifactIdentity(t *testing.T) {
	caps, err := (&Provider{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsImageImport())
	assert.True(t, caps.GetSupportsImageArtifactIdentity())
}

// --- over the wire ------------------------------------------------------------------------

// TestIdentityPrepare_OverTheWire serves the provider over gRPC and drives it
// with the manager's transport client: the echo arrives, and the refusals are
// typed as the manager needs (Conflict, InvalidSpec: hold; retryable: requeue).
func TestIdentityPrepare_OverTheWire(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	c := serveOverGRPC(t, p)
	ctx := context.Background()
	req := func(ovaURL, uid, digest string) contracts.ImagePrepareRequest {
		return contracts.ImagePrepareRequest{
			ImageJSON:    ovaImageJSON(t, ovaURL, nil),
			Image:        testImage(uid),
			SourceDigest: digest,
			Provider:     contracts.ObjectIdentity{UID: testProviderUID, Namespace: "team-a", Name: "vsphere"},
		}
	}

	resp, err := c.PrepareImage(ctx, req(minimalOVAURL(t), testImageUID, testDigestA))
	require.NoError(t, err)
	require.NotNil(t, resp.Artifact)
	assert.Equal(t, testImage(testImageUID).UID, resp.Artifact.Image.UID)
	assert.Equal(t, testDigestA, resp.Artifact.SourceDigest)
	assert.Equal(t, "/DC0/vm/"+resp.Artifact.Name, resp.PreparedImageID)

	plantVM(t, p, defaultVMFolder(t, p), artifactNameFor(t, testOtherUID, testDigestA), nil, true)
	_, err = c.PrepareImage(ctx, req(neverOVA, testOtherUID, testDigestA))
	assert.True(t, contracts.IsConflict(err), "got %v", err)

	_, err = c.PrepareImage(ctx, req(serveBody(t, http.StatusNotFound, nil), testImageUID, testDigestB))
	assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)

	_, err = c.PrepareImage(ctx, req(neverOVA, testImageUID, testDigestB))
	assert.True(t, contracts.IsRetryable(err), "got %v", err)
	assert.False(t, contracts.IsInProgress(err), "an unreachable source is not an import in progress")

	// Another request's unfinished import of this image: typed InProgress,
	// which the manager keeps out of its circuit breaker.
	plantVM(t, p, defaultVMFolder(t, p), artifactNameFor(t, testImageUID, testDigestB),
		stampConfig(testImageUID, testDigestB, time.Now()), false)
	_, err = c.PrepareImage(ctx, req(neverOVA, testImageUID, testDigestB))
	assert.True(t, contracts.IsInProgress(err), "got %v", err)

	// A configured import folder that does not exist: neither a spec problem
	// of the image nor a conflict, retried by the manager.
	p.config.DefaultFolder = "no-such-folder"
	_, err = c.PrepareImage(ctx, req(neverOVA, testOtherUID, testDigestB))
	require.Error(t, err)
	assert.False(t, contracts.IsInvalidSpec(err) || contracts.IsConflict(err) || contracts.IsInProgress(err), "got %v", err)
	assert.Contains(t, err.Error(), "no-such-folder")
}
