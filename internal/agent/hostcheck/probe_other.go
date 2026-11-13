//go:build !linux

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

package hostcheck

import "errors"

// errUnsupported is returned by every System method off Linux.
var errUnsupported = errors.New("the per-host agent runs only on Linux")

// System is the Probe of the running host. Off Linux every method fails, so
// every check refuses the start.
type System struct{}

var _ Probe = System{}

// Lstat implements Probe.
func (System) Lstat(string) (FileInfo, error) { return FileInfo{}, errUnsupported }

// ReadDir implements Probe.
func (System) ReadDir(string) ([]string, error) { return nil, errUnsupported }

// ReadFile implements Probe.
func (System) ReadFile(string, int64) ([]byte, error) { return nil, errUnsupported }

// MountOf implements Probe.
func (System) MountOf(string) (Mount, error) { return Mount{}, errUnsupported }

// NamespaceInode implements Probe.
func (System) NamespaceInode(string) (uint64, error) { return 0, errUnsupported }

// Writable implements Probe.
func (System) Writable(string) (bool, error) { return false, errUnsupported }

// EUID implements Probe.
func (System) EUID() uint32 { return 0 }
