//go:build !virtrigaud_dev

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

package devgate

import (
	"errors"
	"testing"
)

// A release build (no virtrigaud_dev tag) has no gate: naming a type is
// refused with ErrNotCompiled.
func TestResolve_ReleaseBuildRefuses(t *testing.T) {
	if Compiled {
		t.Fatal("Compiled must be false without the virtrigaud_dev tag")
	}
	enabled, err := Resolve("cloudhypervisor")
	if !errors.Is(err, ErrNotCompiled) {
		t.Fatalf("Resolve(cloudhypervisor) = %v, %v; want ErrNotCompiled", enabled, err)
	}
	if enabled != nil {
		t.Fatalf("a refused gate must enable nothing, got %v", enabled)
	}
}
