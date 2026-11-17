//go:build virtrigaud_dev

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

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stringList is a repeatable string flag.
type stringList []string

// String implements flag.Value.
func (s *stringList) String() string { return strings.Join(*s, " ") }

// Set implements flag.Value.
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// imageEpoch is the fixed modification time of every archive entry, so the
// archive is reproducible for a given binary.
var imageEpoch = time.Unix(0, 0).UTC()

// imageUser is the user the image runs as (the manager renders Provider pods
// with this uid anyway).
const imageUser = "65532"

// runImage writes a single-layer image archive in the docker save layout.
func runImage(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("image", flag.ContinueOnError)
	fs.SetOutput(stderr)
	binary := fs.String("binary", "", "static binary to put at /<basename>")
	name := fs.String("name", "", "image reference, e.g. example.invalid/provider:dev")
	out := fs.String("out", "", "archive to write")
	var entryArgs stringList
	fs.Var(&entryArgs, "arg", "entrypoint argument (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *binary == "" || *name == "" || *out == "" {
		return errors.New("-binary, -name and -out are required")
	}
	bin, err := os.ReadFile(filepath.Clean(*binary))
	if err != nil {
		return fmt.Errorf("read the binary: %w", err)
	}
	target := "/" + filepath.Base(*binary)
	layer, err := buildLayer(filepath.Base(*binary), bin)
	if err != nil {
		return err
	}
	layerSum := sha256.Sum256(layer)
	layerHex := hex.EncodeToString(layerSum[:])
	cfg := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      imageEpoch.Format(time.RFC3339),
		"config": map[string]any{
			"Entrypoint": append([]string{target}, entryArgs...),
			"User":       imageUser,
			"Env":        []string{"PATH=/"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + layerHex}},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode the image config: %w", err)
	}
	cfgSum := sha256.Sum256(cfgJSON)
	cfgName := hex.EncodeToString(cfgSum[:]) + ".json"
	manifest, err := json.Marshal([]map[string]any{{
		"Config":   cfgName,
		"RepoTags": []string{*name},
		"Layers":   []string{layerHex + "/layer.tar"},
	}})
	if err != nil {
		return fmt.Errorf("encode the manifest: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{{cfgName, cfgJSON}, {layerHex + "/layer.tar", layer}, {"manifest.json", manifest}} {
		if err := writeTarFile(tw, f.name, f.data, 0o644); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close the archive: %w", err)
	}
	if err := os.WriteFile(filepath.Clean(*out), buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write the archive: %w", err)
	}
	_, _ = fmt.Fprintf(stderr, "image %s: layer sha256:%s config sha256:%s\n", *name, layerHex, strings.TrimSuffix(cfgName, ".json"))
	return nil
}

// buildLayer returns a layer tar holding the binary at /name and an empty
// /tmp.
func buildLayer(name string, bin []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777, ModTime: imageEpoch}); err != nil {
		return nil, fmt.Errorf("write the layer: %w", err)
	}
	if err := writeTarFile(tw, name, bin, 0o755); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close the layer: %w", err)
	}
	return buf.Bytes(), nil
}

// writeTarFile adds one regular file.
func writeTarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data)), ModTime: imageEpoch}); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
