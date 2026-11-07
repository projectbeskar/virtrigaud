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
package journal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// Anchor log record layout. Every record is AnchorRecordSize bytes:
//
//	[0]      format version (anchorFormatVersion; 0 is never valid)
//	[1]      record type (AnchorCommit or AnchorQuarantine)
//	[2:4]    lost-reason code, big endian (quarantine records only)
//	[4:20]   store_id
//	[20:28]  seq, big endian
//	[28:60]  chain head
//	[60:64]  CRC-32C (Castagnoli) of bytes [0:60], big endian
const (
	// AnchorRecordSize is the fixed size of an anchor log record.
	AnchorRecordSize = 64
	// anchorFormatVersion is the record format this package writes.
	anchorFormatVersion = 1
	anchorCRCOffset     = 60
)

// AnchorType is the type of an anchor record.
type AnchorType byte

const (
	// AnchorCommit records the chain position of an acknowledged commit.
	AnchorCommit AnchorType = 1
	// AnchorQuarantine records that the store was quarantined.
	AnchorQuarantine AnchorType = 2
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// AnchorRecord is one decoded anchor log record.
type AnchorRecord struct {
	// Type is the record type.
	Type AnchorType
	// Reason is the lost reason of a quarantine record.
	Reason LostReason
	// StoreID is the store the record belongs to.
	StoreID [16]byte
	// Seq is the chain position.
	Seq uint64
	// Head is the chain head at Seq.
	Head [32]byte
}

// Encode returns the record's AnchorRecordSize bytes.
func (r AnchorRecord) Encode() []byte {
	b := make([]byte, AnchorRecordSize)
	b[0] = anchorFormatVersion
	b[1] = byte(r.Type)
	binary.BigEndian.PutUint16(b[2:4], r.Reason.code())
	copy(b[4:20], r.StoreID[:])
	binary.BigEndian.PutUint64(b[20:28], r.Seq)
	copy(b[28:60], r.Head[:])
	binary.BigEndian.PutUint32(b[anchorCRCOffset:], crc32.Checksum(b[:anchorCRCOffset], castagnoli))
	return b
}

// errBadAnchorRecord reports a record that fails its checks.
var errBadAnchorRecord = errors.New("anchor record invalid")

// DecodeAnchorRecord decodes one record. It fails for a wrong length, an
// unknown version or type, or a CRC mismatch (which includes a zero-filled
// record).
func DecodeAnchorRecord(b []byte) (AnchorRecord, error) {
	var r AnchorRecord
	if len(b) != AnchorRecordSize {
		return r, errBadAnchorRecord
	}
	if binary.BigEndian.Uint32(b[anchorCRCOffset:]) != crc32.Checksum(b[:anchorCRCOffset], castagnoli) {
		return r, errBadAnchorRecord
	}
	if b[0] != anchorFormatVersion {
		return r, errBadAnchorRecord
	}
	r.Type = AnchorType(b[1])
	switch r.Type {
	case AnchorCommit:
	case AnchorQuarantine:
		reason, ok := reasonFromCode(binary.BigEndian.Uint16(b[2:4]))
		if !ok {
			return r, errBadAnchorRecord
		}
		r.Reason = reason
	default:
		return r, errBadAnchorRecord
	}
	copy(r.StoreID[:], b[4:20])
	r.Seq = binary.BigEndian.Uint64(b[20:28])
	copy(r.Head[:], b[28:60])
	return r, nil
}

// AnchorLog is a parsed anchor log.
type AnchorLog struct {
	// Records are the valid records, in file order.
	Records []AnchorRecord
	// ValidLen is the length of the prefix made of valid records.
	ValidLen int
	// TornTail is set when bytes follow ValidLen: one invalid or partial
	// record at the very end, which can only be an unacknowledged append.
	TornTail bool
}

// ErrAnchorCorrupt reports an anchor log with an invalid record that is not
// the last one.
var ErrAnchorCorrupt = errors.New("anchor log corrupt")

// ParseAnchorLog parses an anchor log under the tail rule: an invalid,
// zero-filled or partial record is accepted, as a torn unacknowledged append,
// only when it is the last record of the file. Any other invalid record is
// ErrAnchorCorrupt. It never panics.
func ParseAnchorLog(raw []byte) (AnchorLog, error) {
	var l AnchorLog
	for off := 0; off < len(raw); off += AnchorRecordSize {
		end := off + AnchorRecordSize
		if end > len(raw) {
			// A partial record: torn tail.
			l.TornTail = true
			return l, nil
		}
		r, err := DecodeAnchorRecord(raw[off:end])
		if err != nil {
			if end < len(raw) {
				return AnchorLog{}, ErrAnchorCorrupt
			}
			l.TornTail = true
			return l, nil
		}
		l.Records = append(l.Records, r)
		l.ValidLen = end
	}
	return l, nil
}
