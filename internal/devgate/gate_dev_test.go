//go:build virtrigaud_dev

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
	"testing"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// A development build (virtrigaud_dev tag) enables the named types.
func TestResolve_DevBuildEnables(t *testing.T) {
	if !Compiled {
		t.Fatal("Compiled must be true with the virtrigaud_dev tag")
	}
	enabled, err := Resolve("cloudhypervisor")
	if err != nil {
		t.Fatalf("Resolve(cloudhypervisor): %v", err)
	}
	if _, ok := enabled[infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor]; !ok || len(enabled) != 1 {
		t.Fatalf("Resolve(cloudhypervisor) = %v; want exactly cloudhypervisor", enabled)
	}
}
