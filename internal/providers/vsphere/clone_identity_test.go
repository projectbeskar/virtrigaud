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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R26 on vSphere: a clone of a VM whose cloud-init input is
// the guestinfo keys gets a new instance-id (from its target VirtualMachine's
// UID) and its target's hostname, instead of booting as its source's
// cloud-init instance with the source's SSH host keys.

// guestinfo returns the decoded value of key in extraConfig ("" when absent).
func guestinfo(t *testing.T, extraConfig []types.BaseOptionValue, key string) string {
	t.Helper()
	values := extraConfigValues(extraConfig)
	v, ok := values[key]
	if !ok {
		return ""
	}
	out, err := decodeGuestinfo(v, values[key+".encoding"])
	require.NoError(t, err)
	return out
}

// yamlMap parses a meta-data document.
func yamlMap(t *testing.T, doc string) map[string]any {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &m), "%s", doc)
	return m
}

func TestClone_GivesTheCloneANewCloudInitIdentity(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	create := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	create.UserData = []byte("#cloud-config\nhostname: web\nssh_pwauth: false\n")
	src, err := p.Create(ctx, create)
	require.NoError(t, err)
	_, _, _, srcExtra := vmState(t, p, src.Id)
	require.Equal(t, "web", yamlMap(t, guestinfo(t, srcExtra, guestinfoMetadataKey))["instance-id"], "Create names the instance after the VM")

	resp, err := p.Clone(ctx, cloneReqFor(src.Id, wireOwner(cloneTargetVM)))
	require.NoError(t, err)
	_, _, _, extra := vmState(t, p, resp.TargetVmId)
	meta := yamlMap(t, guestinfo(t, extra, guestinfoMetadataKey))
	assert.Equal(t, "virtrigaud-"+cloneTargetVM.UID, meta["instance-id"], "a new cloud-init instance")
	assert.Equal(t, cloneTargetVM.Name, meta["local-hostname"])
	assert.Equal(t, "#cloud-config\nhostname: \""+cloneTargetVM.Name+"\"\nssh_pwauth: false\n", guestinfo(t, extra, guestinfoUserdataKey),
		"the source's user-data, with the clone's hostname")
	owner, _, _, _ := vmState(t, p, resp.TargetVmId)
	assert.Equal(t, cloneTargetVM, owner, "still stamped with its target")
}

func TestClone_SourceWithoutCloudInitIsUnchanged(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	resp, err := p.Clone(ctx, cloneReqFor(src.Id, wireOwner(cloneTargetVM)))
	require.NoError(t, err)
	_, _, _, extra := vmState(t, p, resp.TargetVmId)
	assert.Empty(t, guestinfo(t, extra, guestinfoMetadataKey), "no guestinfo cloud-init: nothing added")
	assert.Empty(t, guestinfo(t, extra, guestinfoUserdataKey))
}

func TestCloneIdentityExtraConfig(t *testing.T) {
	p := &Provider{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := &providerv1.CloneRequest{TargetName: "copy", TargetVm: &providerv1.ObjectIdentity{Uid: "u-1", Namespace: "ns", Name: "copy"},
		CustomizeJson: `{"hostname":"web2","domain":"lab.example"}`}
	gz := func(s string) string {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, err := w.Write([]byte(s))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	ov := func(k, v string) types.BaseOptionValue { return &types.OptionValue{Key: k, Value: v} }

	t.Run("gzip+base64 meta-data, plain user-data", func(t *testing.T) {
		out, err := p.cloneIdentityExtraConfig([]types.BaseOptionValue{
			ov(guestinfoMetadataKey, gz("instance-id: web\nnetwork:\n  version: 2\n")), ov(guestinfoMetadataEncodingKey, "gz+b64"),
			ov(guestinfoUserdataKey, "#cloud-config\nfqdn: web.corp\n"),
		}, req)
		require.NoError(t, err)
		meta := yamlMap(t, guestinfo(t, out, guestinfoMetadataKey))
		assert.Equal(t, "virtrigaud-u-1", meta["instance-id"])
		assert.Equal(t, "web2.lab.example", meta["local-hostname"])
		assert.NotNil(t, meta["network"], "the rest is kept")
		assert.Equal(t, guestinfoEncodingBase64, extraConfigValues(out)[guestinfoMetadataEncodingKey])
		assert.Equal(t, "#cloud-config\nfqdn: \"web2.lab.example\"\n", guestinfo(t, out, guestinfoUserdataKey))
	})
	t.Run("user-data without hostname keys is not rewritten", func(t *testing.T) {
		out, err := p.cloneIdentityExtraConfig([]types.BaseOptionValue{
			ov(guestinfoMetadataKey, "instance-id: web"), ov(guestinfoUserdataKey, base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\n"))),
			ov(guestinfoUserdataEncodingKey, "base64"),
		}, req)
		require.NoError(t, err)
		_, has := extraConfigValues(out)[guestinfoUserdataKey]
		assert.False(t, has)
		assert.Len(t, out, 2, "only the meta-data pair")
	})
	t.Run("meta-data that cannot be given a new identity refuses the clone", func(t *testing.T) {
		for name, src := range map[string][]types.BaseOptionValue{
			"unknown encoding": {ov(guestinfoMetadataKey, "x"), ov(guestinfoMetadataEncodingKey, "rot13")},
			"bad base64":       {ov(guestinfoMetadataKey, "%%%"), ov(guestinfoMetadataEncodingKey, "base64")},
			"not a mapping":    {ov(guestinfoMetadataKey, "- a\n- b\n")},
		} {
			_, err := p.cloneIdentityExtraConfig(src, req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
		}
	})
	t.Run("a request without a UID", func(t *testing.T) {
		out, err := p.cloneIdentityExtraConfig([]types.BaseOptionValue{ov(guestinfoMetadataKey, "instance-id: web")},
			&providerv1.CloneRequest{TargetName: "copy"})
		require.NoError(t, err)
		meta := yamlMap(t, guestinfo(t, out, guestinfoMetadataKey))
		assert.Equal(t, "virtrigaud-copy", meta["instance-id"])
		assert.Equal(t, "copy", meta["local-hostname"])
	})
}
