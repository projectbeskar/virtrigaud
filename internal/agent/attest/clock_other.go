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

package attest

import "time"

// start is the process's reference point for RawClock off Linux.
var start = time.Now()

// RawClock off Linux is the Go runtime's monotonic clock. The agent runs only
// on Linux; this exists so the package builds on development machines.
type RawClock struct{}

// Now returns the monotonic time since the process started.
func (RawClock) Now() time.Duration { return time.Since(start) }
