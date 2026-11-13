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

package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// CheckKeyDir refuses a key directory that is not a real directory owned by
// uid (the signer's user) or that grants anything to group or others.
func CheckKeyDir(dir string, uid uint32) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("key directory %q is not a clean absolute path", dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat the key directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (or is a symbolic link)", dir)
	}
	owner, ok := fileOwner(fi)
	if !ok || owner != uid {
		return fmt.Errorf("%s is not owned by uid %d", dir, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; it must grant nothing to group or others", dir, fi.Mode().Perm())
	}
	return nil
}

// CreateKey makes the host key at path, for enrolment: it generates an ECDSA
// P-256 key and writes it as PKCS #8 PEM, mode 0600, in a directory owned by
// uid (the signer's user, which must be the caller) that grants nothing to
// group or others. It never replaces a key: when a file exists at path, that
// key is loaded (LoadKey, so it must be the signer's own private file) and
// returned with created false, so that running the enrolment step again
// (a lost CSR file, a new enrolment record) re-signs with the same key. The
// key is written to a private temporary file, synced, and linked into place
// (a link never replaces), then the directory is synced, so a crash leaves
// either no key or a complete one.
//
// Run it as the signer's user: the file belongs to the caller, so the agent's
// user, which is another user, cannot read it.
func CreateKey(path string, uid uint32) (key *ecdsa.PrivateKey, created bool, err error) {
	dir := filepath.Dir(path)
	if err := CheckKeyDir(dir, uid); err != nil {
		return nil, false, err
	}
	removeStrayTemps(dir)
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil && !guard.Off(guard.EnrolKeyNeverReplaced):
		k, err := LoadKey(path, uid)
		return k, false, err
	case statErr == nil:
		// Only the mutation test build gets here: it goes on to replace the key.
	case !errors.Is(statErr, fs.ErrNotExist):
		return nil, false, fmt.Errorf("stat the host key: %w", statErr)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generate the host key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, false, fmt.Errorf("encode the host key: %w", err)
	}
	defer clear(der)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	defer clear(pemBytes)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, false, fmt.Errorf("draw a temporary file name: %w", err)
	}
	tmp := filepath.Join(dir, ".host.key."+hex.EncodeToString(nonce[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- inside the signer's private key directory, name drawn at random, O_EXCL
	if err != nil {
		return nil, false, fmt.Errorf("create the host key file: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(pemBytes); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("write the host key: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("sync the host key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("close the host key: %w", err)
	}
	link := os.Link
	if guard.Off(guard.EnrolKeyNeverReplaced) {
		link = os.Rename // the mutation: replace what is there
	}
	if err := link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another run won: use its key, never ours.
			existing, lerr := LoadKey(path, uid)
			return existing, false, lerr
		}
		return nil, false, fmt.Errorf("install the host key: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, false, fmt.Errorf("sync the key directory: %w", err)
	}
	return k, true, nil
}

// strayTempGlob matches the temporary files CreateKey leaves behind when it
// is killed between creating one and linking it into place.
const strayTempGlob = ".host.key.*"

// removeStrayTemps removes the regular files a killed CreateKey left in dir.
// They hold key material that was never installed. Errors are ignored: a file
// that cannot be removed is not in the way, because every run draws a new
// name.
func removeStrayTemps(dir string) {
	names, err := filepath.Glob(filepath.Join(dir, strayTempGlob))
	if err != nil {
		return
	}
	for _, n := range names {
		if fi, err := os.Lstat(n); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(n)
		}
	}
}

// syncDir syncs a directory so a created or linked entry survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the signer's key directory
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
