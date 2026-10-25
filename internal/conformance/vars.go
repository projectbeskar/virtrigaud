/*
Copyright 2025.

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

package conformance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Built-in variables. The runner sets them; --set cannot override them.
const (
	// VarRun is the run's unique id: six lower-case letters and digits. A
	// specification puts it in the name of every object it creates
	// (for example "vcts-${RUN}-vm"), so each run uses new names.
	VarRun = "RUN"
	// VarTest is the name of the test the step belongs to.
	VarTest = "TEST"
	// VarProvider is the name of the Provider under test (--provider).
	VarProvider = "PROVIDER"
	// VarNamespace is the namespace the run works in (--namespace).
	VarNamespace = "NAMESPACE"
)

// varPattern matches one ${NAME} reference. Names are upper-case letters,
// digits and underscores, starting with a letter.
var varPattern = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

// builtinVars is the set of variable names the runner defines.
var builtinVars = map[string]bool{VarRun: true, VarTest: true, VarProvider: true, VarNamespace: true}

// IsBuiltinVar reports whether name is a variable the runner sets itself.
func IsBuiltinVar(name string) bool {
	return builtinVars[name]
}

// sampleValue is what the validator substitutes for every variable, so a
// name such as "vcts-${RUN}-vm" can be checked against the object-name rules.
const sampleValue = "abc123"

// checkPlaceholders returns an error when s holds a "${" that does not start a
// well-formed ${NAME} reference.
func checkPlaceholders(s string) error {
	rest := varPattern.ReplaceAllString(s, "")
	if strings.Contains(rest, "${") {
		return fmt.Errorf("malformed variable reference in %q: use ${NAME} with NAME of upper-case letters, digits and underscores", s)
	}
	return nil
}

// substituteString replaces every ${NAME} in s with vars[NAME]. Names missing
// from vars are added to missing (when missing is not nil) and left in place.
func substituteString(s string, vars map[string]string, missing map[string]bool) string {
	return varPattern.ReplaceAllStringFunc(s, func(ref string) string {
		name := varPattern.FindStringSubmatch(ref)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		if missing != nil {
			missing[name] = true
		}
		return ref
	})
}

// substitute returns a deep copy of v in which every string value (map keys
// are left alone) has its ${NAME} references replaced. v holds what a YAML
// decoder produces: maps, slices and scalars.
func substitute(v interface{}, vars map[string]string, missing map[string]bool) interface{} {
	switch t := v.(type) {
	case string:
		return substituteString(t, vars, missing)
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = substitute(val, vars, missing)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = substitute(val, vars, missing)
		}
		return out
	default:
		return v
	}
}

// walkStrings calls fn for every string value in v.
func walkStrings(v interface{}, fn func(string) error) error {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]interface{}:
		for _, val := range t {
			if err := walkStrings(val, fn); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, val := range t {
			if err := walkStrings(val, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// stepStrings returns every string of a step that variables are substituted
// in: the resource's values and the assertions' expected values.
func stepStrings(step TestStep) []interface{} {
	vals := []interface{}{step.Resource}
	for _, v := range step.Validate {
		vals = append(vals, v.Value)
	}
	return vals
}

// ReferencedVars returns the sorted names of the variables a test refers to,
// in its steps and its cleanup steps.
func ReferencedVars(test TestSpec) []string {
	seen := map[string]bool{}
	collect := func(s string) error {
		for _, m := range varPattern.FindAllStringSubmatch(s, -1) {
			seen[m[1]] = true
		}
		return nil
	}
	for _, steps := range [][]TestStep{test.Steps, test.Cleanup} {
		for _, step := range steps {
			for _, v := range stepStrings(step) {
				_ = walkStrings(v, collect) // collect never fails
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
