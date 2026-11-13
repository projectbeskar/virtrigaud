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

// Package inventory is the per-host agent's host inventory: what the agent
// observes of the components a host runs virtual machines with, its feature
// probes and the system extensions merged on it, put in the signed host
// answer (HostAnswer.inventory) when the Provider asks for it.
//
// # Collection
//
// A Collector builds an immutable snapshot in the background: at start, every
// interval and after a debounced change in a manifest or extension
// directory. The request path only reads the latest snapshot; it does no
// file system access, hashing or probing, so a caller cannot make the agent
// work by asking.
//
// A component is declared by a manifest (a small JSON file) in the read-only
// image directory or the writable host-configuration directory, and known to
// the agent through its built-in catalog, which names the component, the
// files the agent itself executes or loads, the version range it requires
// and where it can read the installed version. A catalog component present
// without a manifest is UNMANIFESTED. A system extension nobody declares is
// listed as unexpected.
//
// # What the collector never does
//
// It never executes anything found on the host: no component binary, no
// package tool, no version flag. Versions are read as data, from an
// extension-release file, an ELF note or a package database (read by a
// parser in Go; no cgo). Every parser works on bounded input and is fuzzed.
// Digests are computed by the agent over what it finds, never copied from a
// manifest. Nothing it reports is a path, a secret, a command line or a Go
// error string: every reason is a member of a closed set.
//
// # What it is not
//
// The report is a statement by the host's agent, signed so that the Provider
// cannot alter, replay or downgrade it. A host whose agent is compromised
// can report anything, so the inventory is a detective control.
//
// It imports no Kubernetes package.
package inventory
