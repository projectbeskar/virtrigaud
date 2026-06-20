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

package migration

import "testing"

func i64(v int64) *int64 { return &v }

func TestNFSURL_HappyPath(t *testing.T) {
	got, err := NFSURL(StorageOptions{Server: "172.16.56.13", Export: "/export/virtrigaud"}, "mig-abc/disk.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nfs://172.16.56.13/export/virtrigaud/mig-abc/disk.qcow2"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNFSURL_SubpathAndUIDGID(t *testing.T) {
	got, err := NFSURL(StorageOptions{
		Server: "omv.lab", Export: "/export/virtrigaud", Path: "sub/dir",
		UID: i64(65532), GID: i64(65532),
	}, "disk.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	// url.Values.Encode sorts keys: gid before uid.
	if want := "nfs://omv.lab/export/virtrigaud/sub/dir/disk.qcow2?gid=65532&uid=65532"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNFSURL_CleansSlashes(t *testing.T) {
	got, err := NFSURL(StorageOptions{Server: "h", Export: "/export/", Path: "/sub/"}, "/key/")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nfs://h/export/sub/key"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNFSURL_Rejections(t *testing.T) {
	base := StorageOptions{Server: "h", Export: "/export/virtrigaud"}
	cases := []struct {
		name string
		opts StorageOptions
		key  string
	}{
		{"empty server", StorageOptions{Export: "/e"}, "k"},
		{"server with slash", StorageOptions{Server: "h/evil", Export: "/e"}, "k"},
		{"server with query", StorageOptions{Server: "h?uid=0", Export: "/e"}, "k"},
		{"server with amp", StorageOptions{Server: "h&x", Export: "/e"}, "k"},
		{"server with space", StorageOptions{Server: "h x", Export: "/e"}, "k"},
		{"relative export", StorageOptions{Server: "h", Export: "export"}, "k"},
		{"empty export", StorageOptions{Server: "h", Export: ""}, "k"},
		{"key traversal", base, "../../etc/passwd"},
		{"key query", base, "k?uid=0"},
		{"key amp", base, "k&x"},
		{"key fragment", base, "k#frag"},
		{"path traversal", StorageOptions{Server: "h", Export: "/e", Path: "a/../../x"}, "k"},
		{"control char", base, "k\x00"},
	}
	for _, c := range cases {
		if _, err := NFSURL(c.opts, c.key); err == nil {
			t.Errorf("%s: NFSURL = nil err, want rejection", c.name)
		}
	}
}

func TestNFSURL_UIDGIDRange(t *testing.T) {
	if _, err := NFSURL(StorageOptions{Server: "h", Export: "/e", UID: i64(-1)}, "k"); err == nil {
		t.Error("negative uid accepted")
	}
	if _, err := NFSURL(StorageOptions{Server: "h", Export: "/e", GID: i64(4294967296)}, "k"); err == nil {
		t.Error("out-of-range gid accepted")
	}
}

// TestStorageOptions_NFSRoundTrip ensures the NFS coordinates survive the JSON
// round-trip carried in storage_options_json.
func TestStorageOptions_NFSRoundTrip(t *testing.T) {
	in := StorageOptions{Backend: BackendNFS, Server: "omv.lab", Export: "/export/virtrigaud", Path: "p", UID: i64(65532)}
	raw, err := MarshalStorageOptions(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseStorageOptions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Server != in.Server || out.Export != in.Export || out.Path != in.Path ||
		out.UID == nil || *out.UID != 65532 {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}
