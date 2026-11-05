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

// Package devgate is the manager's development gate (--enable-dev-provider-types):
// it decides which development-only provider types
// (infravirtrigaudiov1beta1.IsDevOnlyProviderType) the manager admits and runs.
//
// The gate is compiled in only under the virtrigaud_dev build tag. A build
// without the tag — every release build — has no gate: Resolve refuses any
// non-empty flag value with ErrNotCompiled, and the manager exits at startup
// rather than run without the refusal the release build promises.
//
// Run the tagged tests with: go test -tags virtrigaud_dev ./internal/devgate/ ./cmd/manager/
package devgate

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// BuildTag is the Go build tag that compiles the development gate in.
const BuildTag = "virtrigaud_dev"

// ErrNotCompiled is returned by Resolve when the flag names a type but the
// development gate is not compiled into this build (no BuildTag).
var ErrNotCompiled = errors.New("the development gate is not compiled into this build (it needs the " + BuildTag +
	" build tag); --" + infravirtrigaudiov1beta1.DevProviderTypesFlagName + " cannot be used with a release build")

// validTypes are the provider types the flag accepts.
var validTypes = []infravirtrigaudiov1beta1.ProviderType{infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}

// Parse parses the --enable-dev-provider-types value: a comma-separated list
// of development-only provider types (only "cloudhypervisor" today). Blanks
// around entries are ignored, as are empty entries and repeats; an empty value
// enables none. Any other name is an error. It does not check whether the gate
// is compiled in (Resolve does).
func Parse(value string) ([]infravirtrigaudiov1beta1.ProviderType, error) {
	seen := map[infravirtrigaudiov1beta1.ProviderType]struct{}{}
	var types []infravirtrigaudiov1beta1.ProviderType
	for _, entry := range strings.Split(value, ",") {
		name := strings.TrimSpace(entry)
		if name == "" {
			continue
		}
		t := infravirtrigaudiov1beta1.ProviderType(name)
		if !isValid(t) {
			return nil, fmt.Errorf("parse --%s: %q is not a development-only provider type (valid: %s)",
				infravirtrigaudiov1beta1.DevProviderTypesFlagName, name, validNames())
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		types = append(types, t)
	}
	return types, nil
}

// Resolve parses value (Parse) and returns the set of development-only
// provider types the gate enables. With an empty value it returns an empty
// set in every build. With a non-empty value, a build without the gate
// returns ErrNotCompiled.
func Resolve(value string) (map[infravirtrigaudiov1beta1.ProviderType]struct{}, error) {
	types, err := Parse(value)
	if err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return map[infravirtrigaudiov1beta1.ProviderType]struct{}{}, nil
	}
	return enable(types)
}

// Names renders enabled as a sorted, comma-separated list for logs.
func Names(enabled map[infravirtrigaudiov1beta1.ProviderType]struct{}) string {
	names := make([]string, 0, len(enabled))
	for t := range enabled {
		names = append(names, string(t))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// isValid reports whether t is one of validTypes.
func isValid(t infravirtrigaudiov1beta1.ProviderType) bool {
	for _, v := range validTypes {
		if v == t && infravirtrigaudiov1beta1.IsDevOnlyProviderType(t) {
			return true
		}
	}
	return false
}

// validNames renders validTypes for error messages.
func validNames() string {
	names := make([]string, 0, len(validTypes))
	for _, t := range validTypes {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}
