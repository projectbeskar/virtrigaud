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

package libvirt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
)

// Prepared-image provenance sidecar (ADR-0009 D3).
//
// A libvirt prepared-image artifact <pool>/<name>.qcow2 carries its stamp in a
// dot-sidecar next to it, <pool>/.<name>.virtrigaud-image.json: a small JSON
// document holding the imageartifact.Stamp plus the inode and size of the
// artifact file it stamps. The sidecar is published BEFORE the artifact (see
// image_publish.go), so an artifact that is present always has one, and the
// recorded inode and size must equal the artifact's for the artifact to count
// as complete (ADR-0009 D4): a file swapped in under the artifact name after
// publication no longer matches.
//
// Parsing is strict and fails closed (ADR-0009 D3): an oversized document,
// invalid UTF-8 or JSON, a repeated key (compared case-insensitively, since
// encoding/json matches keys that way), a null anywhere, an unknown field,
// a value of the wrong type, trailing data, an unknown stamp version, an
// implausible image UID, a malformed source digest, or a missing artifact
// inode/size all make the stamp UNTRUSTED — exactly like having no stamp.

const (
	// imageSidecarSuffix ends every sidecar file name:
	// .<artifact base name>.virtrigaud-image.json.
	imageSidecarSuffix = ".virtrigaud-image.json"

	// maxImageSidecarBytes bounds a sidecar document. A larger file is
	// untrusted, and the probe reads at most one byte more than this.
	maxImageSidecarBytes = 4096

	// maxImageSidecarDepth bounds JSON nesting while the sidecar is checked
	// (the real document nests two levels deep).
	maxImageSidecarDepth = 8
)

// imageSidecarName returns the file name of the sidecar of the artifact whose
// base name (without ".qcow2") is name: ".<name>.virtrigaud-image.json". It is
// a dotfile, so #334's reservedImageName refuses it as a base image and a
// directory pool refresh does not list it as a volume.
func imageSidecarName(name string) string {
	return hiddenFilePrefix + name + imageSidecarSuffix
}

// imageSidecarArtifact is the artifact file a sidecar stamps, as stat(1)
// reported it when it was published.
type imageSidecarArtifact struct {
	// Inode is the artifact file's inode number.
	Inode uint64 `json:"inode"`
	// Size is the artifact file's size in bytes.
	Size int64 `json:"size"`
}

// imageSidecar is the sidecar document: the ADR-0009 stamp (its fields are
// promoted to the top level of the JSON object) plus the artifact's inode and
// size.
type imageSidecar struct {
	imageartifact.Stamp
	// Artifact is the artifact file this sidecar stamps.
	Artifact imageSidecarArtifact `json:"artifact"`
}

// validate returns an error unless s can be trusted: a valid stamp
// (imageartifact.Stamp.Validate) and a plausible artifact inode and size.
func (s imageSidecar) validate() error {
	if err := s.Validate(); err != nil { // the embedded imageartifact.Stamp
		return err
	}
	if s.Artifact.Inode == 0 {
		return errors.New("sidecar records no artifact inode")
	}
	if s.Artifact.Size <= 0 {
		return errors.New("sidecar records no artifact size")
	}
	return nil
}

// encodeImageSidecar serializes s as the sidecar document. It refuses a stamp
// it would not itself trust on read, and a document over maxImageSidecarBytes.
func encodeImageSidecar(s imageSidecar) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("encode image sidecar: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode image sidecar: %w", err)
	}
	if len(data) > maxImageSidecarBytes {
		return nil, fmt.Errorf("encode image sidecar: %d bytes exceeds the %d-byte limit", len(data), maxImageSidecarBytes)
	}
	return data, nil
}

// parseImageSidecar strictly parses a sidecar document (see the file comment).
// Any error means the stamp is untrusted.
func parseImageSidecar(data []byte) (imageSidecar, error) {
	if len(data) > maxImageSidecarBytes {
		return imageSidecar{}, fmt.Errorf("sidecar is larger than %d bytes", maxImageSidecarBytes)
	}
	if !utf8.Valid(data) {
		return imageSidecar{}, errors.New("sidecar is not valid UTF-8")
	}
	if err := checkStrictJSON(data); err != nil {
		return imageSidecar{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s imageSidecar
	if err := dec.Decode(&s); err != nil {
		return imageSidecar{}, fmt.Errorf("decode sidecar: %w", err)
	}
	if err := s.validate(); err != nil {
		return imageSidecar{}, err
	}
	return s, nil
}

// checkStrictJSON returns an error unless data is exactly one JSON object with
// no repeated key (case-insensitively) and no null at any level, nested at
// most maxImageSidecarDepth deep, with nothing after it.
func checkStrictJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return fmt.Errorf("sidecar is not JSON: %w", err)
	}
	if d, ok := first.(json.Delim); !ok || d != '{' {
		return errors.New("sidecar is not a JSON object")
	}
	if err := walkStrictJSONObject(dec, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("sidecar has data after its JSON object")
	}
	return nil
}

// walkStrictJSONObject consumes the members of an object whose '{' was just
// read, through its closing '}'.
func walkStrictJSONObject(dec *json.Decoder, depth int) error {
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("sidecar is not JSON: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("sidecar is not JSON: object key is not a string")
		}
		folded := strings.ToLower(key)
		if seen[folded] {
			return fmt.Errorf("sidecar repeats the key %q", key)
		}
		seen[folded] = true
		if err := walkStrictJSONValue(dec, depth); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return fmt.Errorf("sidecar is not JSON: %w", err)
	}
	return nil
}

// walkStrictJSONValue consumes one JSON value.
func walkStrictJSONValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("sidecar is not JSON: %w", err)
	}
	switch t := tok.(type) {
	case nil:
		return errors.New("sidecar contains a null value")
	case json.Delim:
		if depth >= maxImageSidecarDepth {
			return errors.New("sidecar is nested too deeply")
		}
		switch t {
		case '{':
			return walkStrictJSONObject(dec, depth+1)
		case '[':
			for dec.More() {
				if err := walkStrictJSONValue(dec, depth+1); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // the closing ']'
				return fmt.Errorf("sidecar is not JSON: %w", err)
			}
			return nil
		}
		return fmt.Errorf("sidecar is not JSON: unexpected %q", t)
	}
	return nil
}
