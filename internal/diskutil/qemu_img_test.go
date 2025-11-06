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

package diskutil

import (
	"context"
	"testing"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{"1KB", 1024, "1K"},
		{"1MB", 1024 * 1024, "1M"},
		{"1GB", 1024 * 1024 * 1024, "1G"},
		{"10GB", 10 * 1024 * 1024 * 1024, "10G"},
		{"1TB", 1024 * 1024 * 1024 * 1024, "1T"},
		{"512 bytes", 512, "512"},
		{"1.5GB", 1536 * 1024 * 1024, "1G"}, // Rounds down
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatSize(tt.bytes)
			if result != tt.expected {
				t.Errorf("formatSize(%d) = %s, want %s", tt.bytes, result, tt.expected)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{"1KB in bytes", "1024", 1024, false},
		{"1MB in bytes", "1048576", 1048576, false},
		{"1GB in bytes", "1073741824", 1073741824, false},
		{"invalid", "abc", 0, true},
		{"empty", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseBytes(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseBytes(%s) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if result != tt.expected {
				t.Errorf("parseBytes(%s) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseHumanSize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{"1KB", "1K", 1024, false},
		{"1MB", "1M", 1024 * 1024, false},
		{"1GB", "1G", 1024 * 1024 * 1024, false},
		{"10GB", "10G", 10 * 1024 * 1024 * 1024, false},
		{"1.5GB", "1.5G", int64(1.5 * 1024 * 1024 * 1024), false},
		{"1024 bytes", "1024", 1024, false},
		{"1KB uppercase", "1KB", 1024, false},
		{"1MiB", "1MIB", 1024 * 1024, false},
		{"invalid unit", "10X", 0, true},
		{"invalid format", "abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseHumanSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseHumanSize(%s) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if result != tt.expected {
				t.Errorf("parseHumanSize(%s) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseQemuImgInfo(t *testing.T) {
	// Sample output from qemu-img info
	output := `image: test.qcow2
file format: qcow2
virtual size: 10G (10737418240 bytes)
disk size: 196K
cluster_size: 65536
backing file: base.qcow2
encrypted: no`

	result := parseQemuImgInfo(output)

	if result.Format != "qcow2" {
		t.Errorf("Format = %s, want qcow2", result.Format)
	}

	if result.VirtualSize != 10737418240 {
		t.Errorf("VirtualSize = %d, want 10737418240", result.VirtualSize)
	}

	if result.ClusterSize != 65536 {
		t.Errorf("ClusterSize = %d, want 65536", result.ClusterSize)
	}

	if result.BackingFile != "base.qcow2" {
		t.Errorf("BackingFile = %s, want base.qcow2", result.BackingFile)
	}

	if result.Encrypted {
		t.Errorf("Encrypted = true, want false")
	}
}

func TestQemuImgIsInstalled(t *testing.T) {
	q := NewQemuImg()
	// Note: This test will only pass if qemu-img is actually installed
	// In CI/CD, you might want to skip this or mock it
	installed := q.IsInstalled()
	t.Logf("qemu-img installed: %v", installed)

	// We don't fail if not installed, just log it
	// In real tests, you might want to skip tests that require qemu-img
}

func TestConvertOptions_Validation(t *testing.T) {
	q := NewQemuImg()
	ctx := context.Background()

	tests := []struct {
		name    string
		opts    ConvertOptions
		wantErr bool
	}{
		{
			name: "missing source",
			opts: ConvertOptions{
				DestinationPath:   "/tmp/dest.qcow2",
				DestinationFormat: FormatQCOW2,
			},
			wantErr: true,
		},
		{
			name: "missing destination",
			opts: ConvertOptions{
				SourcePath:        "/tmp/source.vmdk",
				DestinationFormat: FormatQCOW2,
			},
			wantErr: true,
		},
		{
			name: "missing destination format",
			opts: ConvertOptions{
				SourcePath:      "/tmp/source.vmdk",
				DestinationPath: "/tmp/dest.qcow2",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := q.Convert(ctx, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("Convert() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewQemuImg(t *testing.T) {
	q := NewQemuImg()
	if q == nil {
		t.Fatal("NewQemuImg() returned nil")
	}
	if q.BinaryPath != "qemu-img" {
		t.Errorf("BinaryPath = %s, want qemu-img", q.BinaryPath)
	}
}

func TestNewQemuImgWithPath(t *testing.T) {
	customPath := "/custom/path/qemu-img"
	q := NewQemuImgWithPath(customPath)
	if q == nil {
		t.Fatal("NewQemuImgWithPath() returned nil")
	}
	if q.BinaryPath != customPath {
		t.Errorf("BinaryPath = %s, want %s", q.BinaryPath, customPath)
	}
}
