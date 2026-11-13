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

package inventory

import (
	"bufio"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Bounds of the version parsers' input.
const (
	// maxExtensionReleaseBytes bounds an extension-release file.
	maxExtensionReleaseBytes = 8 << 10
	// maxELFNoteBytes bounds the note section read from an ELF file.
	maxELFNoteBytes = 64 << 10
	// maxNoteJSONBytes bounds the JSON descriptor of a package note.
	maxNoteJSONBytes = 4 << 10
	// maxPackageDBBytes bounds how much of a package database is read.
	maxPackageDBBytes = 32 << 20
	// maxPackageDBLine bounds a line of a package database.
	maxPackageDBLine = 64 << 10
	// maxVersionSegments and maxSegmentDigits bound a numeric version.
	maxVersionSegments = 8
	maxSegmentDigits   = 9
)

// comparator is one term of a version range.
type comparator struct {
	op  string
	ver []int
}

// versionRange is a conjunction of comparators.
type versionRange []comparator

// parseNumeric parses the leading dotted numeric part of a version ("v1.2.3"
// and "1.2.3+dfsg" give 1, 2, 3). It fails when the version has none.
func parseNumeric(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	var out []int
	i := 0
	for i < len(v) && len(out) < maxVersionSegments {
		j := i
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		if j == i || j-i > maxSegmentDigits {
			break
		}
		n, err := strconv.Atoi(v[i:j])
		if err != nil {
			return nil, false
		}
		out = append(out, n)
		if j < len(v) && v[j] == '.' && j+1 < len(v) && v[j+1] >= '0' && v[j+1] <= '9' {
			i = j + 1
			continue
		}
		break
	}
	return out, len(out) > 0
}

// compareVersions compares numeric versions, a missing segment being zero.
func compareVersions(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// parseRange parses a range such as ">=1.0.0, <2.0.0".
func parseRange(s string) (versionRange, error) {
	var out versionRange
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		op := ""
		for _, candidate := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, candidate) {
				op = candidate
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("range term %q has no comparator", term)
		}
		ver, ok := parseNumeric(strings.TrimSpace(strings.TrimPrefix(term, op)))
		if !ok {
			return nil, fmt.Errorf("range term %q has no version", term)
		}
		out = append(out, comparator{op: op, ver: ver})
	}
	if len(out) == 0 {
		return nil, errors.New("empty range")
	}
	return out, nil
}

// contains reports whether version satisfies every term of the range; a
// version without a numeric part never does.
func (r versionRange) contains(version string) bool {
	v, ok := parseNumeric(version)
	if !ok {
		return false
	}
	for _, c := range r {
		cmp := compareVersions(v, c.ver)
		switch c.op {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		default:
			ok = cmp == 0
		}
		if !ok {
			return false
		}
	}
	return true
}

// extensionReleaseVersion reads the version out of the content of an
// extension-release file: SYSEXT_VERSION_ID, else VERSION_ID. The content is
// data; nothing in it is interpreted beyond KEY=value lines.
func extensionReleaseVersion(content []byte) (string, bool) {
	if len(content) > maxExtensionReleaseBytes {
		return "", false
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		found[strings.TrimSpace(k)] = v
	}
	for _, key := range []string{"SYSEXT_VERSION_ID", "VERSION_ID"} {
		if v := found[key]; versionRE.MatchString(v) {
			return v, true
		}
	}
	return "", false
}

// fdoPackageNote is the type of the packaging metadata note.
const fdoPackageNote = 0xcafe1a7e

// elfPackageVersion reads the version from the packaging metadata note
// (".note.package", owner "FDO") of the ELF file f. It reads only the note
// section, at most maxELFNoteBytes of it, and never loads or runs the file.
func elfPackageVersion(f io.ReaderAt, size int64) (version string, ok bool) {
	defer func() {
		if recover() != nil { // a hostile file must not take the agent down
			version, ok = "", false
		}
	}()
	ef, err := elf.NewFile(io.NewSectionReader(f, 0, size))
	if err != nil {
		return "", false
	}
	sec := ef.Section(".note.package")
	if sec == nil || sec.Type != elf.SHT_NOTE || sec.Size == 0 || sec.Offset > uint64(size) {
		return "", false
	}
	n := min(sec.Size, maxELFNoteBytes)
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(f, int64(sec.Offset), int64(n)), buf); err != nil { // #nosec G115 -- offset bounded by the file size, n by maxELFNoteBytes
		return "", false
	}
	return parsePackageNotes(buf, ef.ByteOrder)
}

// byteOrder is the part of binary.ByteOrder the note parser needs.
type byteOrder interface{ Uint32([]byte) uint32 }

// parsePackageNotes walks the notes of a section and returns the version of
// the first packaging metadata note.
func parsePackageNotes(buf []byte, bo byteOrder) (string, bool) {
	align := func(n uint32) uint64 { return (uint64(n) + 3) &^ 3 }
	for len(buf) >= 12 {
		namesz, descsz, typ := bo.Uint32(buf[0:4]), bo.Uint32(buf[4:8]), bo.Uint32(buf[8:12])
		nameEnd := 12 + align(namesz)
		descEnd := nameEnd + align(descsz)
		if nameEnd > uint64(len(buf)) || descEnd > uint64(len(buf)) {
			return "", false
		}
		name := strings.TrimRight(string(buf[12:12+uint64(namesz)]), "\x00")
		if typ == fdoPackageNote && name == "FDO" && descsz <= maxNoteJSONBytes {
			desc := strings.TrimRight(string(buf[nameEnd:nameEnd+uint64(descsz)]), "\x00")
			var meta struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal([]byte(desc), &meta); err == nil && versionRE.MatchString(meta.Version) {
				return meta.Version, true
			}
			return "", false
		}
		buf = buf[descEnd:]
	}
	return "", false
}

// packageDBVersion reads r (at most maxPackageDBBytes of it) as a Debian
// status file or an Alpine installed database and returns the installed
// version of pkg.
func packageDBVersion(r io.Reader, pkg string) (string, bool) {
	sc := bufio.NewScanner(io.LimitReader(r, maxPackageDBBytes))
	sc.Buffer(make([]byte, 0, 4096), maxPackageDBLine)
	var name, version string
	installed := true
	flush := func() (string, bool) {
		if name == pkg && installed && versionRE.MatchString(version) {
			return version, true
		}
		return "", false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if v, ok := flush(); ok {
				return v, true
			}
			name, version, installed = "", "", true
			continue
		}
		switch {
		case strings.HasPrefix(line, "Package: "):
			name = strings.TrimSpace(line[len("Package: "):])
		case strings.HasPrefix(line, "Version: "):
			version = strings.TrimSpace(line[len("Version: "):])
		case strings.HasPrefix(line, "Status: "):
			installed = strings.HasSuffix(strings.TrimSpace(line), " installed") && !strings.Contains(line, "not-installed")
		case strings.HasPrefix(line, "P:"):
			name = strings.TrimSpace(line[2:])
		case strings.HasPrefix(line, "V:"):
			version = strings.TrimSpace(line[2:])
		}
	}
	if sc.Err() != nil {
		return "", false
	}
	return flush()
}
