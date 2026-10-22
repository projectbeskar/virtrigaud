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
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/cloneidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// A clone's cloud-init identity (R26).
//
// CloneVM_Task copies the source VM's ExtraConfig, and with it the guestinfo
// keys cloud-init's VMware datasource reads: guestinfo.metadata — whose
// instance-id Create sets to the source VM's name — and guestinfo.userdata.
// A clone that kept them would boot as the SOURCE's cloud-init instance (its
// copied disk already records that instance-id), skip every per-instance
// module and keep the source's SSH host keys and hostname. The clone's
// ConfigSpec therefore rewrites them: guestinfo.metadata gets an instance-id
// derived from the target VirtualMachine (cloneidentity.InstanceID) and the
// target's local-hostname, and a #cloud-config guestinfo.userdata gets the
// target's top-level hostname/fqdn. ExtraConfig keys are matched without
// regard to case (vSphere treats them so), and each value is written back
// under the source's own key spelling and in the source's own encoding. A
// source without guestinfo.metadata is left alone: the VMware datasource then
// takes the instance-id from the VM's BIOS UUID, which vCenter regenerates for
// a clone. Guest customization specs and OVF (vApp) properties are not used
// by VirtRigaud and are not touched. cloud-init does not regenerate
// /etc/machine-id on a new instance.

const (
	// guestinfoMetadataKey, guestinfoUserdataKey and their encoding keys are
	// cloud-init's VMware datasource inputs (addCloudInitToConfigSpec).
	guestinfoMetadataKey         = "guestinfo.metadata"
	guestinfoMetadataEncodingKey = "guestinfo.metadata.encoding"
	guestinfoUserdataKey         = "guestinfo.userdata"
	guestinfoUserdataEncodingKey = "guestinfo.userdata.encoding"

	// guestinfoEncodingBase64 / B64 and guestinfoEncodingGzip / Gz are the
	// encodings cloud-init's VMware datasource accepts besides none.
	guestinfoEncodingBase64 = "base64"
	guestinfoEncodingB64    = "b64"
	guestinfoEncodingGzip   = "gzip+base64"
	guestinfoEncodingGz     = "gz+b64"

	// cloneIdentityRefusal is the InvalidArgument answer to a clone whose
	// source's guestinfo.metadata cannot be given a new identity.
	cloneIdentityRefusal = "the clone source's cloud-init guestinfo.metadata cannot be read (unknown encoding or not a " +
		"YAML/JSON mapping), so the clone cannot be given a new cloud-init identity; nothing was cloned"

	// maxGuestinfoBytes bounds a decompressed guestinfo value.
	maxGuestinfoBytes = 4 << 20
)

// guestinfoEntry is one ExtraConfig key as the source spells it, with its
// string value. A zero entry is a key the source does not have.
type guestinfoEntry struct {
	key, value string
}

// cloneIdentityExtraConfig returns the ExtraConfig entries that give a clone
// of a VM with ExtraConfig source its own cloud-init identity (nil when the
// source carries no guestinfo cloud-init keys). The values are never logged:
// user-data may carry secrets.
func (p *Provider) cloneIdentityExtraConfig(source []types.BaseOptionValue, req *providerv1.CloneRequest) ([]types.BaseOptionValue, error) {
	cust, err := cloneidentity.ParseCustomization(req.GetCustomizeJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "the clone's customization is not valid JSON")
	}
	if len(cust.Unapplied) > 0 || cust.InlineUserData != "" || cust.SecretUserData {
		p.logger.Warn("The vSphere provider applies only the hostname and domain of a clone's customization",
			"target_name", req.GetTargetName(), "unapplied", strings.Join(cust.Unapplied, ","),
			"user_data_given", cust.InlineUserData != "" || cust.SecretUserData)
	}
	entries := extraConfigLookup(source)
	target := req.GetTargetVm()
	hostname := cloneidentity.Hostname(cust, contracts.ObjectIdentity{Name: target.GetName()}, req.GetTargetName())

	var out []types.BaseOptionValue
	if md, enc := entries[guestinfoMetadataKey], entries[guestinfoMetadataEncodingKey]; strings.TrimSpace(md.value) != "" {
		meta, err := decodeGuestinfo(md.value, enc.value)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, cloneIdentityRefusal)
		}
		instanceID := cloneidentity.InstanceID(contracts.ObjectIdentity{UID: target.GetUid()}, req.GetTargetName())
		meta, err = cloneidentity.RewriteMetaData(meta, instanceID, cloneidentity.LocalHostname(hostname, cust.Domain))
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, cloneIdentityRefusal)
		}
		v, err := encodeGuestinfo(meta, enc.value)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, cloneIdentityRefusal)
		}
		out = append(out, guestinfoPair(md, enc, v)...)
	}
	if ud, enc := entries[guestinfoUserdataKey], entries[guestinfoUserdataEncodingKey]; strings.TrimSpace(ud.value) != "" {
		user, err := decodeGuestinfo(ud.value, enc.value)
		if err != nil {
			// The user-data is the guest's own business; only the hostname
			// keys would be rewritten. Leave it as it is.
			p.logger.Warn("The clone source's guestinfo.userdata cannot be decoded; its hostname keys are not rewritten",
				"target_name", req.GetTargetName())
			return out, nil
		}
		if keys := cloneidentity.IdentityOverrides(user); len(keys) > 0 {
			p.logger.Warn("The clone source's user-data keeps the source's host identity despite the clone's new instance-id",
				"target_name", req.GetTargetName(), "keys", strings.Join(keys, ","))
		}
		rewritten, changed, skipped := cloneidentity.RewriteUserDataHostname(user, hostname, cust.Domain)
		if len(skipped) > 0 {
			p.logger.Warn("The clone source's user-data sets its hostname in a form the provider does not rewrite",
				"target_name", req.GetTargetName(), "keys", strings.Join(skipped, ","))
		}
		if changed {
			v, err := encodeGuestinfo(rewritten, enc.value)
			if err != nil {
				p.logger.Warn("The clone's rewritten guestinfo.userdata cannot be encoded; it is left as the source's",
					"target_name", req.GetTargetName())
				return out, nil
			}
			out = append(out, guestinfoPair(ud, enc, v)...)
		}
	}
	return out, nil
}

// guestinfoPair is the ExtraConfig a rewritten guestinfo value is written
// with: the value (encoded as the source's encoding key names) under the
// source's key, and that encoding key again, as the source spells and sets
// it, so the pair is consistent whether or not the clone inherits the
// source's ExtraConfig. A source without an encoding key gets none.
func guestinfoPair(value, encoding guestinfoEntry, encoded string) []types.BaseOptionValue {
	out := []types.BaseOptionValue{&types.OptionValue{Key: value.key, Value: encoded}}
	if encoding.key != "" {
		out = append(out, &types.OptionValue{Key: encoding.key, Value: encoding.value})
	}
	return out
}

// extraConfigLookup indexes the string-valued ExtraConfig keys of source by
// their lower-case form (vSphere treats ExtraConfig keys without regard to
// case), keeping each key's own spelling; the first of two spellings wins.
func extraConfigLookup(source []types.BaseOptionValue) map[string]guestinfoEntry {
	out := make(map[string]guestinfoEntry, len(source))
	for _, bov := range source {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil {
			continue
		}
		s, ok := ov.Value.(string)
		if !ok {
			continue
		}
		if lower := strings.ToLower(ov.Key); out[lower].key == "" {
			out[lower] = guestinfoEntry{key: ov.Key, value: s}
		}
	}
	return out
}

// decodeGuestinfo decodes a guestinfo value in one of the encodings
// cloud-init's VMware datasource accepts: none, base64 ("base64", "b64") or
// gzip then base64 ("gzip+base64", "gz+b64").
func decodeGuestinfo(value, encoding string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "":
		return value, nil
	case guestinfoEncodingBase64, guestinfoEncodingB64:
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return "", fmt.Errorf("decode base64 guestinfo: %w", err)
		}
		return string(b), nil
	case guestinfoEncodingGzip, guestinfoEncodingGz:
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return "", fmt.Errorf("decode base64 guestinfo: %w", err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return "", fmt.Errorf("decode gzip guestinfo: %w", err)
		}
		defer zr.Close() //nolint:errcheck // reader over an in-memory buffer
		// guestinfo values are small; bound the inflated size anyway.
		out, err := io.ReadAll(io.LimitReader(zr, maxGuestinfoBytes+1))
		if err != nil {
			return "", fmt.Errorf("decode gzip guestinfo: %w", err)
		}
		if len(out) > maxGuestinfoBytes {
			return "", fmt.Errorf("decoded guestinfo is larger than %d bytes", maxGuestinfoBytes)
		}
		return string(out), nil
	}
	return "", fmt.Errorf("unknown guestinfo encoding %q", encoding)
}

// encodeGuestinfo is the inverse of decodeGuestinfo: value in encoding (the
// source's own, so the clone's keys keep the form its encoding key names).
func encodeGuestinfo(value, encoding string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "":
		return value, nil
	case guestinfoEncodingBase64, guestinfoEncodingB64:
		return base64.StdEncoding.EncodeToString([]byte(value)), nil
	case guestinfoEncodingGzip, guestinfoEncodingGz:
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		if _, err := zw.Write([]byte(value)); err != nil {
			return "", fmt.Errorf("encode gzip guestinfo: %w", err)
		}
		if err := zw.Close(); err != nil {
			return "", fmt.Errorf("encode gzip guestinfo: %w", err)
		}
		return base64.StdEncoding.EncodeToString(b.Bytes()), nil
	}
	return "", fmt.Errorf("unknown guestinfo encoding %q", encoding)
}
