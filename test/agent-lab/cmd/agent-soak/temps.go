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
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
)

// strayRE matches the temporary files an interrupted atomic write leaves:
// a dot, the file's name and 16 lower-case hexadecimal digits.
var strayRE = regexp.MustCompile(`^\..+\.[0-9a-f]{16}$`)

// plantTemps leaves temporary files as a killed write would: in the agent's
// key material directory (owned by the agent) and, when withSigner, beside
// the signer's key (owned by the signer). It returns the paths.
func (l *lab) plantTemps(withSigner bool) []string {
	nonce := func() string { b := make([]byte, 8); _, _ = rand.Read(b); return hex.EncodeToString(b) }
	var out []string
	put := func(dir, name string, a account) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("stray\n"), 0o600); err != nil {
			return
		}
		_ = os.Chown(p, int(a.UID), int(a.GID))
		out = append(out, p)
	}
	pki := filepath.Join(l.Dirs.State, config.PKIDirName)
	put(pki, ".start-epoch."+nonce(), l.Agent)
	put(pki, ".host.crt."+nonce(), l.Agent)
	if withSigner {
		put(l.Dirs.SignerKeyDir, ".host.key."+nonce(), l.Signer)
		put(l.Dirs.SignerKeyDir, ".host.key.pending."+nonce(), l.Signer)
	}
	return out
}

// strayTemps lists the temporary files present in the agent's key material
// directory and beside the signer's key.
func (l *lab) strayTemps() []string {
	var out []string
	for _, d := range []string{filepath.Join(l.Dirs.State, config.PKIDirName), l.Dirs.SignerKeyDir} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strayRE.MatchString(e.Name()) {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}

// tempReport is the result of the temporary file checks.
type tempReport struct {
	Planted        int      `json:"planted"`
	LeftAtEnd      []string `json:"left_at_end,omitempty"`
	SignerRestarts int      `json:"signer_restarts"`
	SignerLeft     int      `json:"signer_temps_left_after_restarts"`
}
