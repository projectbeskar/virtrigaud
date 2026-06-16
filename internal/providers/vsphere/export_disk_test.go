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

package vsphere

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
)

// qemuImgAvailable reports whether qemu-img is on PATH. The flatten path shells
// out to it, so the integration-style tests skip when it is absent (it ships in
// the provider image but may be missing on a bare CI runner).
func qemuImgAvailable() bool {
	return diskutil.NewQemuImg().IsInstalled()
}

// makeMultiFileVMDK builds a genuine multi-file VMDK in dir: a small text
// descriptor ("multi.vmdk") that references a separate data extent
// ("multi-f001.vmdk"). This reproduces exactly what vCenter presents when a
// snapshot of a running VM is exported — the descriptor is tiny and carries no
// disk data on its own. It returns the descriptor path and the SHA256 of the
// raw disk contents so callers can assert data integrity through the flatten.
func makeMultiFileVMDK(t *testing.T, dir string) (descriptorPath, rawSHA256 string) {
	t.Helper()

	qemuImg := "qemu-img"

	// Source raw disk with deterministic, non-zero content so a descriptor-only
	// upload (the bug) would be detectably wrong.
	rawPath := filepath.Join(dir, "base.raw")
	require.NoError(t, exec.Command(qemuImg, "create", "-f", "raw", rawPath, "4M").Run())
	// Random (incompressible) payload so the flattened streamOptimized VMDK is
	// genuinely large — a descriptor-only upload (the bug) would be tiny by
	// contrast, which the size assertions below catch.
	payload := make([]byte, 2*1024*1024)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rawPath, payload, 0o600))
	// Re-extend back to 4M after the truncating write above.
	require.NoError(t, exec.Command(qemuImg, "resize", "-f", "raw", rawPath, "4M").Run())

	sum := sha256.Sum256(rawDiskBytes(t, qemuImg, rawPath))
	rawSHA256 = hex.EncodeToString(sum[:])

	// twoGbMaxExtentFlat forces a descriptor + separate flat extent file, i.e. a
	// multi-file VMDK (descriptor "multi.vmdk" + extent "multi-f001.vmdk").
	descriptorPath = filepath.Join(dir, "multi.vmdk")
	out, err := exec.Command(qemuImg, "convert",
		"-f", "raw", "-O", "vmdk", "-o", "subformat=twoGbMaxExtentFlat",
		rawPath, descriptorPath,
	).CombinedOutput()
	require.NoErrorf(t, err, "qemu-img convert to multi-file vmdk failed: %s", string(out))

	// Sanity: the descriptor must be a separate, tiny file from the extent.
	di, err := os.Stat(descriptorPath)
	require.NoError(t, err)
	require.Less(t, di.Size(), int64(4096), "descriptor should be tiny (this is the bug surface)")
	_, err = os.Stat(filepath.Join(dir, "multi-f001.vmdk"))
	require.NoError(t, err, "extent file must exist alongside the descriptor")

	return descriptorPath, rawSHA256
}

// rawDiskBytes returns the fully-resolved raw bytes of a disk image by asking
// qemu-img to convert it to raw on a temp path and reading the result. Used to
// compare disk *contents* across formats independent of on-disk encoding.
func rawDiskBytes(t *testing.T, qemuImg, imagePath string) []byte {
	t.Helper()
	rawOut := imagePath + ".asraw"
	out, err := exec.Command(qemuImg, "convert", "-O", "raw", imagePath, rawOut).CombinedOutput()
	require.NoErrorf(t, err, "qemu-img convert to raw failed: %s", string(out))
	b, err := os.ReadFile(rawOut)
	require.NoError(t, err)
	_ = os.Remove(rawOut)
	return b
}

// TestFlattenVMDK_MultiFile is the regression test for the ADR-0006 Slice 1 export
// bug: a snapshot export arrives as a tiny descriptor + a separate data extent, and
// the old code uploaded only the descriptor. flattenVMDK must produce a single,
// self-contained VMDK that (a) is much larger than the descriptor and (b) carries
// the exact disk data of the original.
func TestFlattenVMDK_MultiFile(t *testing.T) {
	if !qemuImgAvailable() {
		t.Skip("qemu-img not available; skipping flatten integration test")
	}

	dir := t.TempDir()
	descriptorPath, wantRawSHA := makeMultiFileVMDK(t, dir)

	descInfo, err := os.Stat(descriptorPath)
	require.NoError(t, err)

	flattenedPath := filepath.Join(dir, "flattened.vmdk")
	p := newTestProvider(t)
	require.NoError(t, p.flattenVMDK(context.Background(), descriptorPath, flattenedPath))

	// The flattened file must exist and be substantially larger than the 512-byte
	// descriptor — the whole point of the fix.
	flatInfo, err := os.Stat(flattenedPath)
	require.NoError(t, err)
	assert.Greater(t, flatInfo.Size(), descInfo.Size(),
		"flattened VMDK must be larger than the descriptor (it carries the data)")
	assert.Greater(t, flatInfo.Size(), int64(1024*1024),
		"flattened VMDK should contain the ~2MiB of real disk data")

	// The flattened file must be a single self-contained VMDK whose disk contents
	// match the original raw disk byte-for-byte.
	gotSum := sha256.Sum256(rawDiskBytes(t, "qemu-img", flattenedPath))
	assert.Equal(t, wantRawSHA, hex.EncodeToString(gotSum[:]),
		"flattened disk contents must match the original")
}

// TestFlattenVMDK_SingleFile verifies the defensive single-file case: flattening a
// VMDK that is already self-contained is a safe re-encode and still yields a valid,
// single, self-contained VMDK with intact contents. (In ExportDisk this case is
// normally short-circuited to upload-as-is, but flattenVMDK must remain correct if
// called.)
func TestFlattenVMDK_SingleFile(t *testing.T) {
	if !qemuImgAvailable() {
		t.Skip("qemu-img not available; skipping flatten integration test")
	}

	dir := t.TempDir()
	qemuImg := "qemu-img"

	rawPath := filepath.Join(dir, "base.raw")
	require.NoError(t, exec.Command(qemuImg, "create", "-f", "raw", rawPath, "4M").Run())
	payload := make([]byte, 1*1024*1024)
	for i := range payload {
		payload[i] = byte((i * 7) % 253)
	}
	require.NoError(t, os.WriteFile(rawPath, payload, 0o600))
	require.NoError(t, exec.Command(qemuImg, "resize", "-f", "raw", rawPath, "4M").Run())
	wantSum := sha256.Sum256(rawDiskBytes(t, qemuImg, rawPath))

	// monolithicSparse is a single self-contained VMDK (no separate extent file).
	singlePath := filepath.Join(dir, "single.vmdk")
	out, err := exec.Command(qemuImg, "convert",
		"-f", "raw", "-O", "vmdk", "-o", "subformat=monolithicSparse",
		rawPath, singlePath,
	).CombinedOutput()
	require.NoErrorf(t, err, "qemu-img convert to single-file vmdk failed: %s", string(out))

	flattenedPath := filepath.Join(dir, "flattened.vmdk")
	p := newTestProvider(t)
	require.NoError(t, p.flattenVMDK(context.Background(), singlePath, flattenedPath))

	_, err = os.Stat(flattenedPath)
	require.NoError(t, err)
	gotSum := sha256.Sum256(rawDiskBytes(t, qemuImg, flattenedPath))
	assert.Equal(t, hex.EncodeToString(wantSum[:]), hex.EncodeToString(gotSum[:]),
		"single-file flatten must preserve disk contents")
}

// TestFlattenVMDK_QemuImgMissing verifies flattenVMDK fails loudly (rather than
// silently producing nothing/uploading a descriptor) when qemu-img is not on PATH.
// PATH is temporarily emptied so diskutil.NewQemuImg().IsInstalled() returns false.
func TestFlattenVMDK_QemuImgMissing(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	require.NoError(t, os.Setenv("PATH", ""))

	dir := t.TempDir()
	descriptorPath := filepath.Join(dir, "disk.vmdk")
	require.NoError(t, os.WriteFile(descriptorPath, []byte("# Disk DescriptorFile\n"), 0o600))

	p := newTestProvider(t)
	err := p.flattenVMDK(context.Background(), descriptorPath, filepath.Join(dir, "flattened.vmdk"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "qemu-img is not available")
}
