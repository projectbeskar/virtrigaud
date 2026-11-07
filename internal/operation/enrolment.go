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

package operation

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DevEnrolmentFileFlagName is the manager flag that names the development
// enrolment file (LoadEnrolments). Only a manager built with the
// virtrigaud_dev build tag accepts it; a release build refuses to start when
// it is set.
const DevEnrolmentFileFlagName = "dev-operation-enrolment-file"

// ErrNoEnrolment is returned by an EnrolmentSource that has no enrolment for
// a Provider's host. Nothing is signed or sent for that host.
var ErrNoEnrolment = errors.New("no operation-identity enrolment is configured for this host")

// Enrolment is what the manager's grants for one host carry about the host's
// enrolment: its 16-byte enrolment identifier and the epoch the grants are
// issued under.
type Enrolment struct {
	// EnrolmentID is the host's 16-byte enrolment identifier.
	EnrolmentID []byte
	// Epoch is the epoch grants for the host are issued under; an op_id is
	// minted at it too. Counter is at least 1.
	Epoch Epoch
}

// EnrolmentSource returns the enrolment of a Provider's host. It is a
// development-only STAND-IN for an enrolment the host proves with a signed
// answer: StaticEnrolments, read from a development enrolment file. Its
// epochs are identity-only values with no ordering or fencing meaning.
type EnrolmentSource interface {
	// Enrolment returns the enrolment of host hostID of the Provider
	// providerNamespace/providerName, or an error wrapping ErrNoEnrolment.
	Enrolment(providerNamespace, providerName, hostID string) (Enrolment, error)
}

// StaticEnrolments is an EnrolmentSource over a fixed table: one epoch for
// every host and, per Provider (namespace/name), each host's enrolment
// identifier. A development-only stand-in (see EnrolmentSource).
type StaticEnrolments struct {
	// Epoch is the epoch of every enrolment.
	Epoch Epoch
	// Hosts maps "namespace/name" of a Provider to its hosts' enrolment
	// identifiers, by host id.
	Hosts map[string]map[string][]byte
}

// Compile-time assertion that StaticEnrolments is an EnrolmentSource.
var _ EnrolmentSource = StaticEnrolments{}

// Enrolment implements EnrolmentSource. The returned slices are copies.
func (s StaticEnrolments) Enrolment(providerNamespace, providerName, hostID string) (Enrolment, error) {
	id, ok := s.Hosts[providerNamespace+"/"+providerName][hostID]
	if !ok || len(id) != IDLen || s.Epoch.Counter == 0 || len(s.Epoch.Holder) != IDLen {
		return Enrolment{}, fmt.Errorf("host %q of Provider %s/%s: %w", hostID, providerNamespace, providerName, ErrNoEnrolment)
	}
	return Enrolment{
		EnrolmentID: append([]byte(nil), id...),
		Epoch:       Epoch{Counter: s.Epoch.Counter, Holder: append([]byte(nil), s.Epoch.Holder...)},
	}, nil
}

// enrolmentFile is the JSON form of a development enrolment file:
//
//	{"epoch": {"counter": 2, "holder": "<32 hex>"},
//	 "providers": {"<namespace>/<name>": {"<host id>": "<32 hex>"}}}
type enrolmentFile struct {
	Epoch struct {
		Counter uint64 `json:"counter"`
		Holder  string `json:"holder"`
	} `json:"epoch"`
	Providers map[string]map[string]string `json:"providers"`
}

// ParseEnrolments parses a development enrolment file (see enrolmentFile).
// Unknown fields are refused; every identifier must be 32 hexadecimal
// characters, every Provider key "namespace/name", and the epoch counter at
// least 1.
func ParseEnrolments(data []byte) (StaticEnrolments, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var f enrolmentFile
	if err := dec.Decode(&f); err != nil {
		return StaticEnrolments{}, fmt.Errorf("parse development enrolment file: %w", err)
	}
	if dec.More() {
		return StaticEnrolments{}, errors.New("parse development enrolment file: trailing data after the JSON object")
	}
	if f.Epoch.Counter == 0 {
		return StaticEnrolments{}, errors.New("parse development enrolment file: epoch.counter must be at least 1")
	}
	holder, err := decodeID(f.Epoch.Holder)
	if err != nil {
		return StaticEnrolments{}, fmt.Errorf("parse development enrolment file: epoch.holder: %w", err)
	}
	out := StaticEnrolments{Epoch: Epoch{Counter: f.Epoch.Counter, Holder: holder}, Hosts: map[string]map[string][]byte{}}
	for key, hosts := range f.Providers {
		ns, name, ok := strings.Cut(key, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return StaticEnrolments{}, fmt.Errorf("parse development enrolment file: provider key %q is not namespace/name", key)
		}
		out.Hosts[key] = map[string][]byte{}
		for host, hexID := range hosts {
			if host == "" {
				return StaticEnrolments{}, fmt.Errorf("parse development enrolment file: provider %s has an empty host id", key)
			}
			id, err := decodeID(hexID)
			if err != nil {
				return StaticEnrolments{}, fmt.Errorf("parse development enrolment file: provider %s host %s: %w", key, host, err)
			}
			out.Hosts[key][host] = id
		}
	}
	return out, nil
}

// decodeID decodes a 16-byte identifier written as 32 hexadecimal characters.
func decodeID(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != IDLen {
		return nil, fmt.Errorf("not %d hexadecimal characters", 2*IDLen)
	}
	return b, nil
}
