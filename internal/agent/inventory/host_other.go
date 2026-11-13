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

package inventory

import "errors"

// ErrUnsupported is returned by SystemHost off Linux.
var ErrUnsupported = errors.New("the host inventory probes need Linux")

// SystemHost is the Host of a non-Linux machine: every probe fails, so a
// development machine builds and reports ERROR for each feature.
type SystemHost struct{}

var _ Host = SystemHost{}

// NamespaceInode implements Host.
func (SystemHost) NamespaceInode(string) (uint64, error) { return 0, ErrUnsupported }

// Cgroup2Mounted implements Host.
func (SystemHost) Cgroup2Mounted() (bool, error) { return false, ErrUnsupported }

// KernelRelease implements Host.
func (SystemHost) KernelRelease() (string, error) { return "", ErrUnsupported }

// PidfdOpenSelf implements Host.
func (SystemHost) PidfdOpenSelf() error { return ErrUnsupported }

// PidfsHandle implements Host.
func (SystemHost) PidfsHandle() (bool, error) { return false, ErrUnsupported }

// PidfdOnPidfs implements Host.
func (SystemHost) PidfdOnPidfs() (bool, error) { return false, ErrUnsupported }

// FSMagic implements Host.
func (SystemHost) FSMagic(string) (uint64, error) { return 0, ErrUnsupported }
