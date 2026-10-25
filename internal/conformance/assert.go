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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/jsonpath"
)

// Assertion operators (Validation.Operator).
const (
	OpEq       = "eq"
	OpNe       = "ne"
	OpGt       = "gt"
	OpLt       = "lt"
	OpGte      = "gte"
	OpLte      = "lte"
	OpContains = "contains"
	OpMatches  = "matches"
	OpExists   = "exists"
)

// Wait conditions (WaitCondition.Condition).
const (
	// CondReady: the object's Ready condition is True and, when the condition
	// records an observedGeneration, it is not older than the object's
	// metadata.generation.
	CondReady = "Ready"
	// CondAvailable: as CondReady, for the Available condition.
	CondAvailable = "Available"
	// CondRunning: a VirtualMachine whose status.powerState is On.
	CondRunning = "Running"
	// CondStopped: a VirtualMachine whose status.powerState is Off.
	CondStopped = "Stopped"
	// CondProvisioned: a VirtualMachine with a non-empty status.id.
	CondProvisioned = "Provisioned"
	// CondFailed: the object's status.phase is Failed.
	CondFailed = "Failed"
	// CondDeleted: the object does not exist.
	CondDeleted = "Deleted"
)

// vmOnlyConditions are the wait conditions defined for VirtualMachine only.
var vmOnlyConditions = map[string]bool{CondRunning: true, CondStopped: true, CondProvisioned: true}

// phaseFailed is the status.phase value every VirtRigaud kind with a phase
// uses for a failure.
const phaseFailed = "Failed"

// compilePath parses a Validation path ("." or "$" followed by a kubectl
// JSONPath expression, e.g. .status.conditions[?(@.type=="Ready")].status).
func compilePath(path string) (*jsonpath.JSONPath, error) {
	if path == "" || (path[0] != '.' && path[0] != '$') {
		return nil, errors.New("path must start with '.' or '$'")
	}
	jp := jsonpath.New("vcts").AllowMissingKeys(true)
	if err := jp.Parse("{" + path + "}"); err != nil {
		return nil, fmt.Errorf("invalid path %q: %w", path, err)
	}
	return jp, nil
}

// lookup returns the values path resolves to in obj; nulls are left out.
func lookup(obj map[string]interface{}, path string) ([]interface{}, error) {
	jp, err := compilePath(path)
	if err != nil {
		return nil, err
	}
	results, err := jp.FindResults(obj)
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", path, err)
	}
	var vals []interface{}
	for _, set := range results {
		for _, rv := range set {
			if !rv.IsValid() {
				continue
			}
			if (rv.Kind() == reflect.Interface || rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Ptr) && rv.IsNil() {
				continue
			}
			vals = append(vals, rv.Interface())
		}
	}
	return vals, nil
}

// evaluate checks one assertion against obj. The returned error says what
// was found.
func evaluate(obj map[string]interface{}, v Validation) error {
	vals, err := lookup(obj, v.Path)
	if err != nil {
		return err
	}
	op := v.Operator
	if op == "" {
		op = OpEq
	}

	if op == OpExists {
		want := true
		if v.Value != nil {
			b, ok := v.Value.(bool)
			if !ok {
				return fmt.Errorf("%s exists: value must be true or false, got %v", v.Path, v.Value)
			}
			want = b
		}
		if found := len(vals) > 0; found != want {
			if want {
				return fmt.Errorf("%s: not set", v.Path)
			}
			return fmt.Errorf("%s: want it not set, got %s", v.Path, show(vals))
		}
		return nil
	}

	if len(vals) == 0 {
		return fmt.Errorf("%s: not set (want %s %s)", v.Path, op, show1(v.Value))
	}

	if op == OpContains {
		if containsValue(vals, v.Value) {
			return nil
		}
		return fmt.Errorf("%s: %s does not contain %s", v.Path, show(vals), show1(v.Value))
	}

	if len(vals) != 1 {
		return fmt.Errorf("%s: resolves to %d values %s; operator %s needs exactly one", v.Path, len(vals), show(vals), op)
	}
	got := vals[0]

	var ok bool
	switch op {
	case OpEq:
		ok = equal(got, v.Value)
	case OpNe:
		ok = !equal(got, v.Value)
	case OpGt, OpLt, OpGte, OpLte:
		ok, err = compareNumbers(got, v.Value, op)
		if err != nil {
			return fmt.Errorf("%s: %w", v.Path, err)
		}
	case OpMatches:
		pattern, isString := v.Value.(string)
		if !isString {
			return fmt.Errorf("%s matches: value must be a regular expression string", v.Path)
		}
		re, reErr := regexp.Compile(pattern)
		if reErr != nil {
			return fmt.Errorf("%s matches: invalid regular expression: %w", v.Path, reErr)
		}
		ok = re.MatchString(scalarString(got))
	default:
		return fmt.Errorf("unknown operator %q", op)
	}
	if !ok {
		return fmt.Errorf("%s: got %s, want %s %s", v.Path, show1(got), op, show1(v.Value))
	}
	return nil
}

// equal compares an observed value with an expected one: numerically when
// both are numbers, otherwise by their string forms (so "2" equals 2 and
// true equals "true").
func equal(got, want interface{}) bool {
	if g, ok := toNumber(got); ok {
		if w, ok := toNumber(want); ok {
			return g == w
		}
	}
	return scalarString(got) == scalarString(want)
}

// containsValue reports whether want is one of vals, an element of the list
// vals[0] when it is the only value, or a substring of a string value.
func containsValue(vals []interface{}, want interface{}) bool {
	if len(vals) == 1 {
		switch t := vals[0].(type) {
		case []interface{}:
			vals = t
		case string:
			if s, ok := want.(string); ok {
				return strings.Contains(t, s)
			}
		}
	}
	for _, v := range vals {
		if equal(v, want) {
			return true
		}
	}
	return false
}

// compareNumbers evaluates got <op> want for the ordering operators. Both
// must be numbers or Kubernetes quantities ("2Gi").
func compareNumbers(got, want interface{}, op string) (bool, error) {
	g, ok := toNumber(got)
	if !ok {
		return false, fmt.Errorf("%s is not a number or quantity", show1(got))
	}
	w, ok := toNumber(want)
	if !ok {
		return false, fmt.Errorf("expected value %s is not a number or quantity", show1(want))
	}
	switch op {
	case OpGt:
		return g > w, nil
	case OpLt:
		return g < w, nil
	case OpGte:
		return g >= w, nil
	default: // OpLte
		return g <= w, nil
	}
}

// toNumber converts a number, or a string holding a number or a Kubernetes
// quantity, to a float64.
func toNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f, true
		}
		if q, err := resource.ParseQuantity(t); err == nil {
			return q.AsApproximateFloat64(), true
		}
	}
	return 0, false
}

// scalarString renders v as a plain string: strings as they are, everything
// else as compact JSON.
func scalarString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// show1 renders one value for an error message.
func show1(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// show renders values for an error message.
func show(vals []interface{}) string {
	if len(vals) == 1 {
		return show1(vals[0])
	}
	return show1(vals)
}

// conditionMet reports whether obj satisfies the wait condition cond, and a
// short description of what it saw. The object exists (CondDeleted is
// handled by the caller).
func conditionMet(obj *unstructured.Unstructured, cond string) (bool, string) {
	switch cond {
	case CondReady, CondAvailable:
		return statusConditionTrue(obj, cond)
	case CondRunning, CondStopped:
		want := "On"
		if cond == CondStopped {
			want = "Off"
		}
		got, _, _ := unstructured.NestedString(obj.Object, "status", "powerState")
		return got == want, fmt.Sprintf("status.powerState=%q", got)
	case CondProvisioned:
		id, _, _ := unstructured.NestedString(obj.Object, "status", "id")
		return id != "", fmt.Sprintf("status.id=%q", id)
	case CondFailed:
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		return phase == phaseFailed, fmt.Sprintf("status.phase=%q", phase)
	case CondDeleted:
		return false, "still exists"
	default:
		return false, fmt.Sprintf("unknown condition %q", cond)
	}
}

// statusConditionTrue reports whether the status condition of type condType
// is True and not stale: a condition whose observedGeneration is set must
// have observed at least the object's metadata.generation.
func statusConditionTrue(obj *unstructured.Unstructured, condType string) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok || m["type"] != condType {
			continue
		}
		status, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		message, _ := m["message"].(string)
		desc := fmt.Sprintf("%s=%s reason=%q message=%q", condType, status, reason, message)
		if status != "True" {
			return false, desc
		}
		if observed, ok := toNumber(m["observedGeneration"]); ok && observed > 0 &&
			int64(observed) < obj.GetGeneration() {
			return false, fmt.Sprintf("%s (observedGeneration %d < generation %d)", desc, int64(observed), obj.GetGeneration())
		}
		return true, desc
	}
	return false, fmt.Sprintf("no %s condition", condType)
}

// failedPhase returns a description of obj's failure when its status.phase
// is Failed, and "" otherwise.
func failedPhase(obj *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if phase != phaseFailed {
		return ""
	}
	msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")
	_, readyDesc := statusConditionTrue(obj, CondReady)
	return fmt.Sprintf("status.phase=Failed message=%q %s", msg, readyDesc)
}
