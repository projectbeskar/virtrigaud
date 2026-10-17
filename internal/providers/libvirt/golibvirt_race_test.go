//go:build race

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

package libvirt

// knownUpstreamRace is true when the test binary is built with the race
// detector. In that build the go-libvirt data race documented in golibvirt.go
// ("Known upstream race") can be reported by any test that stresses connects
// and evictions concurrently, so those tests skip themselves; see
// skipOnKnownUpstreamRace.
const knownUpstreamRace = true
