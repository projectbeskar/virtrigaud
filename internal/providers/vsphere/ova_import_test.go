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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/ovf/importer"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func optVal(key, value string) types.BaseOptionValue {
	return &types.OptionValue{Key: key, Value: value}
}

func optionKeys(opts []types.BaseOptionValue) []string {
	var keys []string
	for _, bov := range opts {
		keys = append(keys, bov.GetOptionValue().Key)
	}
	return keys
}

func TestStripReservedExtraConfig(t *testing.T) {
	t.Run("virtual machine spec", func(t *testing.T) {
		spec := &types.VirtualMachineImportSpec{ConfigSpec: types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			optVal("guestinfo.userdata", "x"),
			optVal(ownerExtraConfigKeyUID, "victim-uid"),
			nil,
			optVal("Virtrigaud.Owner.Name", "web"), // VMX keys are case-insensitive
			optVal("disk.EnableUUID", "TRUE"),
			optVal("virtrigaud.anything", "y"),
			optVal("not.virtrigaud.owner.uid", "kept"), // only the prefix is reserved
		}}}

		removed := stripReservedExtraConfig(spec)
		assert.ElementsMatch(t, []string{ownerExtraConfigKeyUID, "Virtrigaud.Owner.Name", "virtrigaud.anything"}, removed)
		assert.Equal(t, []string{"guestinfo.userdata", "disk.EnableUUID", "not.virtrigaud.owner.uid"}, optionKeys(spec.ConfigSpec.ExtraConfig),
			"unreserved keys are kept, in order")
	})

	t.Run("vApp spec is walked recursively", func(t *testing.T) {
		vm1 := &types.VirtualMachineImportSpec{ConfigSpec: types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			optVal(ownerExtraConfigKeyUID, "victim-uid"), optVal("a", "1"),
		}}}
		vm2 := &types.VirtualMachineImportSpec{ConfigSpec: types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
			optVal(ownerExtraConfigKeyNamespace, "team-a"),
		}}}
		spec := &types.VirtualAppImportSpec{Child: []types.BaseImportSpec{
			vm1, &types.VirtualAppImportSpec{Child: []types.BaseImportSpec{vm2}},
		}}

		removed := stripReservedExtraConfig(spec)
		assert.ElementsMatch(t, []string{ownerExtraConfigKeyUID, ownerExtraConfigKeyNamespace}, removed)
		assert.Equal(t, []string{"a"}, optionKeys(vm1.ConfigSpec.ExtraConfig))
		assert.Empty(t, vm2.ConfigSpec.ExtraConfig)
	})

	t.Run("nothing reserved", func(t *testing.T) {
		spec := &types.VirtualMachineImportSpec{ConfigSpec: types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{optVal("a", "1")}}}
		assert.Empty(t, stripReservedExtraConfig(spec))
		assert.Equal(t, []string{"a"}, optionKeys(spec.ConfigSpec.ExtraConfig))
	})
}

// ovfExtraConfigInjector wraps the vim25 round-tripper and appends extra
// entries to the ExtraConfig of every CreateImportSpec result, emulating
// vCenter mapping an OVF's <vmw:ExtraConfig> elements into the import spec
// (vcsim's CreateImportSpec does not map them).
type ovfExtraConfigInjector struct {
	soap.RoundTripper
	extra []types.BaseOptionValue
}

func (r *ovfExtraConfigInjector) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	if body, ok := res.(*methods.CreateImportSpecBody); ok && body.Res != nil {
		if spec, ok := body.Res.Returnval.ImportSpec.(*types.VirtualMachineImportSpec); ok {
			spec.ConfigSpec.ExtraConfig = append(spec.ConfigSpec.ExtraConfig, r.extra...)
		}
	}
	return nil
}

func extraConfigOf(t *testing.T, p *Provider, ref types.ManagedObjectReference) map[string]string {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), ref,
		[]string{"config.extraConfig"}, &vm))
	out := make(map[string]string)
	for _, bov := range vm.Config.ExtraConfig {
		o := bov.GetOptionValue()
		if s, ok := o.Value.(string); ok {
			out[strings.ToLower(o.Key)] = s
		}
	}
	return out
}

// TestImagePrepare_OVAImport_StripsReservedExtraConfig: an OVA whose OVF
// carries virtrigaud.owner.* ExtraConfig (a forged owner stamp) is imported
// WITHOUT those keys, while its other ExtraConfig is kept.
func TestImagePrepare_OVAImport_StripsReservedExtraConfig(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()
	ctx := context.Background()

	p.client.RoundTripper = &ovfExtraConfigInjector{
		RoundTripper: p.client.RoundTripper,
		extra: []types.BaseOptionValue{
			optVal(ownerExtraConfigKeyUID, ownerTeamA.UID),
			optVal("VirtRigaud.Owner.Namespace", ownerTeamA.Namespace),
			optVal("guestinfo.keep", "yes"),
		},
	}

	ovaURL, _, closeSrv := newOVATarServer(t)
	defer closeSrv()

	// Control: govmomi's stock importer (no stripping) imports the forged stamp,
	// so the assertion below can only pass because ImagePrepare strips it.
	localPath, cleanupDownload, err := p.downloadOVA(ctx, ovaURL)
	require.NoError(t, err)
	defer cleanupDownload()
	archive, descriptor, err := p.newOVAArchive(localPath, ovaURL)
	require.NoError(t, err)
	placement, err := p.resolveImagePlacement(ctx, "")
	require.NoError(t, err)
	controlName := "control-import"
	stock := &importer.Importer{
		Log: p.ovaImportLog, Client: p.client.Client, Finder: p.finder, Datacenter: placement.datacenter,
		Datastore: placement.datastore, ResourcePool: placement.resourcePool, Folder: placement.folder, Archive: archive,
	}
	controlRef, err := stock.Import(ctx, descriptor, importer.Options{Name: &controlName, DiskProvisioning: ovaDiskProvisioningThin})
	require.NoError(t, err)
	require.Equal(t, ownerTeamA.UID, extraConfigOf(t, p, *controlRef)[ownerExtraConfigKeyUID],
		"control: without stripping, the OVF's forged owner stamp is imported")

	const targetName = "tenant-image"
	resp, err := p.ImagePrepare(ctx, &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"ovaURL":"` + ovaURL + `"}}}`,
		TargetName: targetName,
	})
	require.NoError(t, err)
	require.Equal(t, targetName, resp.GetPreparedImageId())

	vm, found, err := p.lookupTemplate(ctx, targetName)
	require.NoError(t, err)
	require.True(t, found)
	extra := extraConfigOf(t, p, vm.Reference())
	for key := range extra {
		assert.False(t, isReservedExtraConfigKey(key), "reserved key %q must not be imported", key)
	}
	assert.Equal(t, "yes", extra["guestinfo.keep"], "unreserved OVF ExtraConfig is still imported")
}
