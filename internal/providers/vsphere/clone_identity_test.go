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
	"strings"
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
	entries := extraConfigLookup(extraConfig)
	e, ok := entries[key]
	if !ok {
		return ""
	}
	out, err := decodeGuestinfo(e.value, entries[key+".encoding"].value)
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

// applied is source with out applied over it, as the clone's ConfigSpec is
// applied to the ExtraConfig the clone copies (keys compared without regard
// to case).
func applied(source, out []types.BaseOptionValue) []types.BaseOptionValue {
	res := append([]types.BaseOptionValue(nil), source...)
	for _, o := range out {
		ov := o.GetOptionValue()
		replaced := false
		for i, s := range res {
			if strings.EqualFold(s.GetOptionValue().Key, ov.Key) {
				res[i], replaced = o, true
			}
		}
		if !replaced {
			res = append(res, o)
		}
	}
	return res
}

// keysOf lists the keys of out.
func keysOf(out []types.BaseOptionValue) []string {
	var keys []string
	for _, o := range out {
		keys = append(keys, o.GetOptionValue().Key)
	}
	return keys
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

	// Review L6: each value is written back in the source's own encoding.
	t.Run("gzip+base64 meta-data stays gzip, plain user-data stays plain", func(t *testing.T) {
		src := []types.BaseOptionValue{
			ov(guestinfoMetadataKey, gz("instance-id: web\nnetwork:\n  version: 2\n")), ov(guestinfoMetadataEncodingKey, "gz+b64"),
			ov(guestinfoUserdataKey, "#cloud-config\nfqdn: web.corp\n"),
		}
		out, err := p.cloneIdentityExtraConfig(src, req)
		require.NoError(t, err)
		assert.Equal(t, []string{guestinfoMetadataKey, guestinfoMetadataEncodingKey, guestinfoUserdataKey}, keysOf(out),
			"the source's encoding key restated; plain user-data gets none")
		assert.Equal(t, "gz+b64", extraConfigLookup(out)[guestinfoMetadataEncodingKey].value)
		raw, err := base64.StdEncoding.DecodeString(extraConfigLookup(out)[guestinfoMetadataKey].value)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x1f, 0x8b}, raw[:2], "still gzip")

		res := applied(src, out)
		meta := yamlMap(t, guestinfo(t, res, guestinfoMetadataKey))
		assert.Equal(t, "virtrigaud-u-1", meta["instance-id"])
		assert.Equal(t, "web2.lab.example", meta["local-hostname"])
		assert.NotNil(t, meta["network"], "the rest is kept")
		assert.Equal(t, "#cloud-config\nfqdn: \"web2.lab.example\"\n", guestinfo(t, res, guestinfoUserdataKey),
			"plain user-data written back plain")
	})
	t.Run("base64 and b64 stay base64", func(t *testing.T) {
		for _, enc := range []string{"base64", "b64", "BASE64"} {
			src := []types.BaseOptionValue{ov(guestinfoMetadataKey, base64.StdEncoding.EncodeToString([]byte("instance-id: web\n"))),
				ov(guestinfoMetadataEncodingKey, enc)}
			out, err := p.cloneIdentityExtraConfig(src, req)
			require.NoError(t, err, enc)
			assert.Equal(t, "virtrigaud-u-1", yamlMap(t, guestinfo(t, applied(src, out), guestinfoMetadataKey))["instance-id"], enc)
		}
	})
	// Review L5: keys are matched without regard to case and written back
	// under the source's spelling.
	t.Run("keys in another case", func(t *testing.T) {
		src := []types.BaseOptionValue{
			ov("GuestInfo.MetaData", base64.StdEncoding.EncodeToString([]byte("instance-id: web\n"))),
			ov("GUESTINFO.METADATA.ENCODING", "base64"),
			ov("guestInfo.userData", "#cloud-config\nhostname: web\n"),
		}
		out, err := p.cloneIdentityExtraConfig(src, req)
		require.NoError(t, err)
		assert.Equal(t, []string{"GuestInfo.MetaData", "GUESTINFO.METADATA.ENCODING", "guestInfo.userData"}, keysOf(out),
			"the source's spelling")
		res := applied(src, out)
		assert.Len(t, res, 3, "no second spelling of a key is added")
		assert.Equal(t, "virtrigaud-u-1", yamlMap(t, guestinfo(t, res, guestinfoMetadataKey))["instance-id"])
		assert.Equal(t, "#cloud-config\nhostname: \"web2\"\n", guestinfo(t, res, guestinfoUserdataKey))
	})
	t.Run("user-data without hostname keys is not rewritten", func(t *testing.T) {
		out, err := p.cloneIdentityExtraConfig([]types.BaseOptionValue{
			ov(guestinfoMetadataKey, "instance-id: web"), ov(guestinfoUserdataKey, base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\n"))),
			ov(guestinfoUserdataEncodingKey, "base64"),
		}, req)
		require.NoError(t, err)
		assert.Equal(t, []string{guestinfoMetadataKey}, keysOf(out), "only the meta-data")
	})
	t.Run("meta-data that cannot be given a new identity refuses the clone", func(t *testing.T) {
		for name, src := range map[string][]types.BaseOptionValue{
			"unknown encoding": {ov(guestinfoMetadataKey, "x"), ov(guestinfoMetadataEncodingKey, "rot13")},
			"bad base64":       {ov(guestinfoMetadataKey, "%%%"), ov(guestinfoMetadataEncodingKey, "base64")},
			"not a mapping":    {ov(guestinfoMetadataKey, "- a\n- b\n")},
			"an anchored id":   {ov(guestinfoMetadataKey, "instance-id: &i web\nother: *i\n")},
		} {
			_, err := p.cloneIdentityExtraConfig(src, req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
		}
	})
	t.Run("a request without a UID", func(t *testing.T) {
		src := []types.BaseOptionValue{ov(guestinfoMetadataKey, "instance-id: web")}
		out, err := p.cloneIdentityExtraConfig(src, &providerv1.CloneRequest{TargetName: "copy"})
		require.NoError(t, err)
		meta := yamlMap(t, guestinfo(t, applied(src, out), guestinfoMetadataKey))
		assert.Equal(t, "virtrigaud-copy", meta["instance-id"])
		assert.Equal(t, "copy", meta["local-hostname"])
	})
}

func TestEncodeGuestinfoRoundTrip(t *testing.T) {
	for _, enc := range []string{"", "base64", "b64", "gzip+base64", "gz+b64", "Gzip+Base64"} {
		v, err := encodeGuestinfo("instance-id: x\n", enc)
		require.NoError(t, err, enc)
		back, err := decodeGuestinfo(v, enc)
		require.NoError(t, err, enc)
		assert.Equal(t, "instance-id: x\n", back, enc)
	}
	_, err := encodeGuestinfo("x", "rot13")
	assert.Error(t, err)
}

