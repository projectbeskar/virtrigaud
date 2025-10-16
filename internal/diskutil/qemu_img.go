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
	"fmt"
	"os/exec"
	"strings"
)

// SupportedFormat represents a disk image format supported by qemu-img
type SupportedFormat string

const (
	FormatQCOW2 SupportedFormat = "qcow2"
	FormatVMDK  SupportedFormat = "vmdk"
	FormatRaw   SupportedFormat = "raw"
	FormatVDI   SupportedFormat = "vdi"
	FormatVHD   SupportedFormat = "vpc" // VirtualPC/VHD format
	FormatVHDX  SupportedFormat = "vhdx"
)

// ConvertOptions holds options for disk image conversion
type ConvertOptions struct {
	// SourcePath is the path to the source disk image
	SourcePath string

	// DestinationPath is the path for the converted disk image
	DestinationPath string

	// SourceFormat is the format of the source image (auto-detected if empty)
	SourceFormat SupportedFormat

	// DestinationFormat is the target format for conversion
	DestinationFormat SupportedFormat

	// Compression enables compression for formats that support it (qcow2, vmdk)
	Compression bool

	// ProgressCallback is called with progress updates (0-100)
	ProgressCallback func(percent int)
}

// InfoResult contains information about a disk image
type InfoResult struct {
	// Format is the disk image format
	Format string

	// VirtualSize is the virtual size of the disk in bytes
	VirtualSize int64

	// ActualSize is the actual size on disk in bytes
	ActualSize int64

	// ClusterSize is the cluster size (for qcow2)
	ClusterSize int64

	// Encrypted indicates if the image is encrypted
	Encrypted bool

	// BackingFile is the path to the backing file (for snapshots)
	BackingFile string
}

// QemuImg provides utilities for working with disk images using qemu-img
type QemuImg struct {
	// BinaryPath is the path to the qemu-img binary
	BinaryPath string
}

// NewQemuImg creates a new QemuImg instance
func NewQemuImg() *QemuImg {
	return &QemuImg{
		BinaryPath: "qemu-img", // Assumes qemu-img is in PATH
	}
}

// NewQemuImgWithPath creates a new QemuImg instance with a custom binary path
func NewQemuImgWithPath(binaryPath string) *QemuImg {
	return &QemuImg{
		BinaryPath: binaryPath,
	}
}

// Convert converts a disk image from one format to another
func (q *QemuImg) Convert(ctx context.Context, opts ConvertOptions) error {
	if opts.SourcePath == "" {
		return fmt.Errorf("source path is required")
	}
	if opts.DestinationPath == "" {
		return fmt.Errorf("destination path is required")
	}
	if opts.DestinationFormat == "" {
		return fmt.Errorf("destination format is required")
	}

	// Build qemu-img convert command
	args := []string{"convert"}

	// Add progress monitoring
	args = append(args, "-p")

	// Add source format if specified
	if opts.SourceFormat != "" {
		args = append(args, "-f", string(opts.SourceFormat))
	}

	// Add destination format
	args = append(args, "-O", string(opts.DestinationFormat))

	// Add compression if requested and supported
	if opts.Compression {
		switch opts.DestinationFormat {
		case FormatQCOW2:
			args = append(args, "-c")
		case FormatVMDK:
			args = append(args, "-o", "subformat=streamOptimized")
		}
	}

	// Add source and destination paths
	args = append(args, opts.SourcePath, opts.DestinationPath)

	// Execute command
	cmd := exec.CommandContext(ctx, q.BinaryPath, args...)
	
	// Capture output for progress tracking
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert failed: %w, output: %s", err, string(output))
	}

	return nil
}

// Info retrieves information about a disk image
func (q *QemuImg) Info(ctx context.Context, imagePath string) (*InfoResult, error) {
	if imagePath == "" {
		return nil, fmt.Errorf("image path is required")
	}

	// Execute qemu-img info command
	cmd := exec.CommandContext(ctx, q.BinaryPath, "info", imagePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info failed: %w, output: %s", err, string(output))
	}

	// Parse the output
	return parseQemuImgInfo(string(output)), nil
}

// Create creates a new disk image
func (q *QemuImg) Create(ctx context.Context, imagePath string, format SupportedFormat, sizeBytes int64) error {
	if imagePath == "" {
		return fmt.Errorf("image path is required")
	}
	if format == "" {
		return fmt.Errorf("format is required")
	}
	if sizeBytes <= 0 {
		return fmt.Errorf("size must be positive")
	}

	// Convert size to qemu-img format (e.g., "10G")
	sizeStr := formatSize(sizeBytes)

	// Execute qemu-img create command
	args := []string{"create", "-f", string(format), imagePath, sizeStr}
	cmd := exec.CommandContext(ctx, q.BinaryPath, args...)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create failed: %w, output: %s", err, string(output))
	}

	return nil
}

// Resize changes the size of a disk image
func (q *QemuImg) Resize(ctx context.Context, imagePath string, newSizeBytes int64) error {
	if imagePath == "" {
		return fmt.Errorf("image path is required")
	}
	if newSizeBytes <= 0 {
		return fmt.Errorf("new size must be positive")
	}

	sizeStr := formatSize(newSizeBytes)

	// Execute qemu-img resize command
	cmd := exec.CommandContext(ctx, q.BinaryPath, "resize", imagePath, sizeStr)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img resize failed: %w, output: %s", err, string(output))
	}

	return nil
}

// Check performs consistency checks on a disk image
func (q *QemuImg) Check(ctx context.Context, imagePath string, repair bool) error {
	if imagePath == "" {
		return fmt.Errorf("image path is required")
	}

	args := []string{"check"}
	if repair {
		args = append(args, "-r", "all")
	}
	args = append(args, imagePath)

	cmd := exec.CommandContext(ctx, q.BinaryPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img check failed: %w, output: %s", err, string(output))
	}

	return nil
}

// IsInstalled checks if qemu-img is available
func (q *QemuImg) IsInstalled() bool {
	cmd := exec.Command(q.BinaryPath, "--version")
	err := cmd.Run()
	return err == nil
}

// GetVersion returns the qemu-img version
func (q *QemuImg) GetVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, q.BinaryPath, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get qemu-img version: %w", err)
	}

	// Parse version from output (first line usually contains version)
	lines := strings.Split(string(output), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0]), nil
	}

	return "", fmt.Errorf("unable to parse version from output")
}

// parseQemuImgInfo parses the output of `qemu-img info`
func parseQemuImgInfo(output string) *InfoResult {
	result := &InfoResult{}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Split by colon
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "file format":
			result.Format = value
		case "virtual size":
			// Parse "10G (10737418240 bytes)" format
			if idx := strings.Index(value, "("); idx != -1 {
				sizeStr := strings.TrimSpace(value[idx+1:])
				sizeStr = strings.TrimSuffix(sizeStr, " bytes)")
				sizeStr = strings.TrimSuffix(sizeStr, ")")
				if size, err := parseBytes(sizeStr); err == nil {
					result.VirtualSize = size
				}
			}
		case "disk size":
			// Parse disk size
			if size, err := parseHumanSize(value); err == nil {
				result.ActualSize = size
			}
		case "cluster_size":
			if size, err := parseBytes(value); err == nil {
				result.ClusterSize = size
			}
		case "encrypted":
			result.Encrypted = strings.ToLower(value) == "yes"
		case "backing file":
			result.BackingFile = value
		}
	}

	return result
}

// formatSize formats bytes into qemu-img size format (e.g., "10G")
func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%dT", bytes/TB)
	case bytes >= GB:
		return fmt.Sprintf("%dG", bytes/GB)
	case bytes >= MB:
		return fmt.Sprintf("%dM", bytes/MB)
	case bytes >= KB:
		return fmt.Sprintf("%dK", bytes/KB)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}

// parseBytes parses a byte string (e.g., "10737418240") to int64
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	var value int64
	_, err := fmt.Sscanf(s, "%d", &value)
	return value, err
}

// parseHumanSize parses human-readable size (e.g., "10G", "1.5M") to bytes
func parseHumanSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	// Extract number and unit
	var value float64
	var unit string
	n, err := fmt.Sscanf(s, "%f%s", &value, &unit)
	if err != nil || n < 1 {
		// Try without unit (raw bytes)
		var rawValue int64
		_, err := fmt.Sscanf(s, "%d", &rawValue)
		if err == nil {
			return rawValue, nil
		}
		return 0, fmt.Errorf("invalid size format: %s", s)
	}

	// If no unit, assume bytes
	if n == 1 {
		return int64(value), nil
	}

	// Convert based on unit
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)

	switch unit {
	case "K", "KB", "KIB":
		return int64(value * KB), nil
	case "M", "MB", "MIB":
		return int64(value * MB), nil
	case "G", "GB", "GIB":
		return int64(value * GB), nil
	case "T", "TB", "TIB":
		return int64(value * TB), nil
	default:
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}
}

