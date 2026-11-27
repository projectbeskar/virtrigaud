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

package controller

// The host ledger keys (development builds only): the keys that tag the
// manager's host ledger objects and their index, and that key the host
// identity a ledger object is bound to.
//
// They are random keys of their own, kept apart from every other key of the
// manager: they are not derived from the operation grant signer or from the
// operation-intent key (a release build has no signer, and a key held by a
// custody service cannot be exported to derive from). They come from a
// directory, normally a Secret volume mounted only into the manager pod:
//
//	current   the current tag key: 64 lowercase hexadecimal characters
//	          (32 random bytes)
//	previous  optional: the tag key current before the last rotation
//	identity  the host identity key: 64 lowercase hexadecimal characters
//
// A tag key's id is the first 8 bytes of a SHA-256 over a fixed context and
// the key, as 16 hexadecimal characters, so an id always names the key that
// made it. A tag is verified with the key its id names (current or previous)
// and every write tags with the current key, so a rotation (move current to
// previous, put a new key in current, restart the manager) never fails a
// tag and re-tags each object on its next write. The identity key is never
// rotated: the identity MAC stored in a ledger object is computed with it,
// and a ledger object whose identity MAC no longer matches is refused for
// its Host.
//
// Nothing here logs, formats or returns key material.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// DevHostLedgerKeyDirFlagName names the directory holding the host ledger
// keys (normally a Secret volume mounted only into the manager pod). Only a
// development build honours it; a release build refuses to start when it is
// set. It needs per-host agent enrolment.
const DevHostLedgerKeyDirFlagName = "dev-host-ledger-key-dir"

// File names in the host ledger key directory (also the data keys of the
// Secret mounted there).
const (
	// HostLedgerKeyCurrentFile holds the current tag key.
	HostLedgerKeyCurrentFile = "current"
	// HostLedgerKeyPreviousFile holds the tag key current before the last
	// rotation; optional.
	HostLedgerKeyPreviousFile = "previous"
	// HostLedgerKeyIdentityFile holds the host identity key.
	HostLedgerKeyIdentityFile = "identity"
)

// hostLedgerKeyLen is the length of every host ledger key in bytes.
const hostLedgerKeyLen = 32

// hostLedgerKeyIDLen is the length of a tag key id in bytes (16 hexadecimal
// characters).
const hostLedgerKeyIDLen = 8

// Contexts of the host ledger key family. Each starts a MAC or digest input,
// so a value computed for one purpose never verifies for another.
const (
	// hostLedgerKeyIDContext derives a tag key's id.
	hostLedgerKeyIDContext = "virtrigaud.io/host-ledger-key-id/v1\x00"
	// hostLedgerTagContext tags a ledger object.
	hostLedgerTagContext = "virtrigaud.io/host-ledger/v1\x00"
	// hostLedgerIndexTagContext tags the ledger index of a pins Secret.
	hostLedgerIndexTagContext = "virtrigaud.io/host-ledger-index/v1\x00"
	// hostIdentityMACContext keys a Host's identity.
	hostIdentityMACContext = "virtrigaud.io/host-identity/v1\x00"
)

// errHostLedgerKeys wraps every refusal of a host ledger key directory.
var errHostLedgerKeys = errors.New("invalid host ledger keys")

// HostLedgerKeys are the manager's host ledger keys. The zero value holds no
// key and is not usable; LoadHostLedgerKeys returns a usable one. Its
// methods never expose key material; String and GoString name only the key
// ids.
type HostLedgerKeys struct {
	currentID   string
	current     []byte
	previousID  string
	previous    []byte
	identityKey []byte
}

// String implements fmt.Stringer without key material.
func (k *HostLedgerKeys) String() string {
	if k == nil {
		return "HostLedgerKeys(none)"
	}
	if k.previousID == "" {
		return fmt.Sprintf("HostLedgerKeys(current %s)", k.currentID)
	}
	return fmt.Sprintf("HostLedgerKeys(current %s, previous %s)", k.currentID, k.previousID)
}

// GoString implements fmt.GoStringer without key material.
func (k *HostLedgerKeys) GoString() string { return k.String() }

// hostLedgerKeyID returns the id of a tag key.
func hostLedgerKeyID(key []byte) string {
	h := sha256.New()
	h.Write([]byte(hostLedgerKeyIDContext))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:hostLedgerKeyIDLen])
}

// NewHostLedgerKeys returns the keys from their raw bytes (previous may be
// nil). Every key must be 32 bytes, not all zero, and all must differ.
func NewHostLedgerKeys(current, previous, identity []byte) (*HostLedgerKeys, error) {
	check := func(name string, key []byte) error {
		if len(key) != hostLedgerKeyLen {
			return fmt.Errorf("%w: the %s key must be %d bytes", errHostLedgerKeys, name, hostLedgerKeyLen)
		}
		if bytes.Equal(key, make([]byte, hostLedgerKeyLen)) {
			return fmt.Errorf("%w: the %s key is all zero", errHostLedgerKeys, name)
		}
		return nil
	}
	if err := check(HostLedgerKeyCurrentFile, current); err != nil {
		return nil, err
	}
	if err := check(HostLedgerKeyIdentityFile, identity); err != nil {
		return nil, err
	}
	if hmac.Equal(current, identity) {
		return nil, fmt.Errorf("%w: the identity key must differ from the tag keys", errHostLedgerKeys)
	}
	k := &HostLedgerKeys{currentID: hostLedgerKeyID(current), current: bytes.Clone(current), identityKey: bytes.Clone(identity)}
	if previous != nil {
		if err := check(HostLedgerKeyPreviousFile, previous); err != nil {
			return nil, err
		}
		if hmac.Equal(previous, current) {
			return nil, fmt.Errorf("%w: the previous tag key equals the current one", errHostLedgerKeys)
		}
		if hmac.Equal(previous, identity) {
			return nil, fmt.Errorf("%w: the identity key must differ from the tag keys", errHostLedgerKeys)
		}
		k.previousID, k.previous = hostLedgerKeyID(previous), bytes.Clone(previous)
	}
	return k, nil
}

// readHostLedgerKeyFile reads one key file: 64 lowercase hexadecimal
// characters, with surrounding white space ignored. A missing optional file
// is (nil, nil).
func readHostLedgerKeyFile(dir, name string, optional bool) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- an administrator-chosen key directory
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: read the %s key file: %w", errHostLedgerKeys, name, err)
	}
	text := strings.TrimSpace(string(raw))
	if len(text) != 2*hostLedgerKeyLen || strings.ToLower(text) != text {
		return nil, fmt.Errorf("%w: the %s key file must hold %d lowercase hexadecimal characters", errHostLedgerKeys, name,
			2*hostLedgerKeyLen)
	}
	key, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: the %s key file must hold %d lowercase hexadecimal characters", errHostLedgerKeys, name,
			2*hostLedgerKeyLen)
	}
	return key, nil
}

// LoadHostLedgerKeys reads the host ledger keys from dir (see the file
// comment for its layout). An empty dir returns nil and no error (no ledger
// keys). A release build refuses any dir, and the keys need per-host agent
// enrolment (enrolment true).
func LoadHostLedgerKeys(dir string, enrolment bool) (*HostLedgerKeys, error) {
	if dir == "" {
		return nil, nil
	}
	if !devgate.Compiled {
		return nil, fmt.Errorf("--%s cannot be used with a release build: %w", DevHostLedgerKeyDirFlagName, devgate.ErrNotCompiled)
	}
	if !enrolment {
		return nil, fmt.Errorf("--%s needs per-host agent enrolment (--%s)", DevHostLedgerKeyDirFlagName, DevHostEnrolmentNamespaceFlagName)
	}
	current, err := readHostLedgerKeyFile(dir, HostLedgerKeyCurrentFile, false)
	if err != nil {
		return nil, err
	}
	previous, err := readHostLedgerKeyFile(dir, HostLedgerKeyPreviousFile, true)
	if err != nil {
		return nil, err
	}
	identity, err := readHostLedgerKeyFile(dir, HostLedgerKeyIdentityFile, false)
	if err != nil {
		return nil, err
	}
	return NewHostLedgerKeys(current, previous, identity)
}

// CurrentKeyID returns the id of the current tag key, under which every
// write is tagged.
func (k *HostLedgerKeys) CurrentKeyID() string { return k.currentID }

// PreviousKeyID returns the id of the previous tag key, or "".
func (k *HostLedgerKeys) PreviousKeyID() string { return k.previousID }

// tagKey returns the tag key named by id, or false for an unknown id.
func (k *HostLedgerKeys) tagKey(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	switch id {
	case k.currentID:
		return k.current, true
	case k.previousID:
		return k.previous, true
	}
	return nil, false
}

// tag returns the HMAC-SHA256 under the current tag key of context ‖ msg,
// and the key's id.
func (k *HostLedgerKeys) tag(context string, msg []byte) (id string, sum [sha256.Size]byte) {
	m := hmac.New(sha256.New, k.current)
	m.Write([]byte(context))
	m.Write(msg)
	copy(sum[:], m.Sum(nil))
	return k.currentID, sum
}

// verifyTag reports whether sum is the HMAC-SHA256 of context ‖ msg under
// the tag key id names. An unknown id never verifies.
func (k *HostLedgerKeys) verifyTag(id, context string, msg, sum []byte) bool {
	key, ok := k.tagKey(id)
	if !ok {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(context))
	m.Write(msg)
	return hmac.Equal(m.Sum(nil), sum)
}

// IdentityMAC returns the HMAC-SHA256 under the identity key of a Host's
// identity (Host.spec.identity) within the installation installID: the
// value that binds a ledger object to a machine, and whose prefix labels the
// object, without revealing a low-entropy identity such as a serial number
// to whoever can list Secrets.
func (k *HostLedgerKeys) IdentityMAC(installID []byte, identity string) [sha256.Size]byte {
	m := hmac.New(sha256.New, k.identityKey)
	m.Write([]byte(hostIdentityMACContext))
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(installID))) // #nosec G115 -- an install id is 16 bytes
	m.Write(n[:])
	m.Write(installID)
	m.Write([]byte(identity))
	var sum [sha256.Size]byte
	copy(sum[:], m.Sum(nil))
	return sum
}
