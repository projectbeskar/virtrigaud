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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sweepKinds are the kinds the cleanup lists by the run and test labels, in
// this order, to find objects created on the test's behalf that the runner
// did not create itself (a VMClone's target VirtualMachine). Kinds the test
// created are swept too.
var sweepKinds = []string{kindVirtualMachine, "VMSnapshot", "VMClone"}

// trackedObject is an object a test created.
type trackedObject struct {
	gvk  schema.GroupVersionKind
	name string
	uid  types.UID
}

// String renders the object as Kind/name.
func (o trackedObject) String() string {
	return o.gvk.Kind + "/" + o.name
}

// testRun is the state of one test while it runs.
type testRun struct {
	r    *Runner
	test TestSpec
	vars map[string]string

	mu      sync.Mutex
	tracked []trackedObject
}

// newTestRun returns the state for running test: its variables are the
// user's plus the built-in ones.
func (r *Runner) newTestRun(test TestSpec) *testRun {
	vars := make(map[string]string, len(r.config.Vars)+4)
	for k, v := range r.config.Vars {
		vars[k] = v
	}
	vars[VarRun] = r.config.RunID
	vars[VarTest] = test.Name
	vars[VarProvider] = r.config.Provider
	vars[VarNamespace] = r.config.Namespace
	return &testRun{r: r, test: test, vars: vars}
}

// pollInterval returns how often a wait step reads its object.
func (t *testRun) pollInterval() time.Duration {
	if t.r.config.PollInterval > 0 {
		return t.r.config.PollInterval
	}
	return DefaultPollInterval
}

// stepTimeout returns the timeout of step.
func stepTimeout(step TestStep) time.Duration {
	if step.Type == StepWait {
		if step.WaitFor != nil {
			if d, err := time.ParseDuration(step.WaitFor.Timeout); err == nil && d > 0 {
				return d
			}
		}
		if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 {
			return d
		}
		return DefaultWaitTimeout
	}
	if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultStepTimeout
}

// runStep executes one step. In cleanup, deleting an object that is already
// gone is not a failure.
func (t *testRun) runStep(ctx context.Context, step TestStep, cleanup bool) StepResult {
	start := time.Now()
	result := StepResult{Name: step.Name}

	err := t.execStep(ctx, step, cleanup)
	if err != nil {
		result.Status = StatusFailed
		result.Error = err.Error()
	} else {
		result.Status = StatusPassed
	}
	result.Duration = time.Since(start)
	return result
}

// execStep substitutes the step's variables and performs it.
func (t *testRun) execStep(ctx context.Context, step TestStep, cleanup bool) error {
	resource, ok := substitute(step.Resource, t.vars, nil).(map[string]interface{})
	if !ok {
		return errors.New("resource is not a mapping")
	}
	validations := make([]Validation, len(step.Validate))
	for i, v := range step.Validate {
		validations[i] = v
		validations[i].Value = substitute(v.Value, t.vars, nil)
	}
	gvk := gvkOf(resource)
	name, _ := nestedString(resource, "metadata", "name")

	stepCtx, cancel := context.WithTimeout(ctx, stepTimeout(step))
	defer cancel()

	switch step.Type {
	case StepCreate:
		if err := t.create(stepCtx, resource); err != nil {
			return err
		}
		return t.validateOnce(stepCtx, gvk, name, validations)
	case StepUpdate:
		if err := t.update(stepCtx, gvk, name, resource); err != nil {
			return err
		}
		return t.validateOnce(stepCtx, gvk, name, validations)
	case StepDelete:
		return t.delete(stepCtx, gvk, name, cleanup)
	case StepWait:
		cond := ""
		if step.WaitFor != nil {
			cond = step.WaitFor.Condition
		}
		return t.waitFor(stepCtx, gvk, name, cond, validations)
	case StepValidate:
		return t.validateOnce(stepCtx, gvk, name, validations)
	default:
		return fmt.Errorf("unknown step type: %s", step.Type)
	}
}

// newObject returns an empty object of gvk named name in the run's
// namespace, to Get into.
func (t *testRun) newObject(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(t.r.config.Namespace)
	u.SetName(name)
	return u
}

// get reads the object of gvk named name.
func (t *testRun) get(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	u := t.newObject(gvk, name)
	if err := t.r.config.KubeClient.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		return nil, err
	}
	return u, nil
}

// create creates resource in the run's namespace with the run's labels and
// tracks it for cleanup.
func (t *testRun) create(ctx context.Context, resource map[string]interface{}) error {
	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("encode resource: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("decode resource: %w", err)
	}
	u.SetNamespace(t.r.config.Namespace)
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelRun] = t.r.config.RunID
	labels[LabelTest] = t.test.Name
	labels[LabelManagedBy] = managedByValue
	u.SetLabels(labels)

	if err := t.r.config.KubeClient.Create(ctx, u); err != nil {
		return fmt.Errorf("create %s/%s: %w", u.GetKind(), u.GetName(), err)
	}
	t.mu.Lock()
	t.tracked = append(t.tracked, trackedObject{gvk: u.GroupVersionKind(), name: u.GetName(), uid: u.GetUID()})
	t.mu.Unlock()
	return nil
}

// ownedByRun returns an error unless obj carries this run's label: the
// runner changes and deletes only objects of its own run.
func (t *testRun) ownedByRun(obj *unstructured.Unstructured) error {
	if obj.GetLabels()[LabelRun] != t.r.config.RunID {
		return fmt.Errorf("%s/%s does not carry the label %s=%s: vcts only changes or deletes objects its own run created",
			obj.GetKind(), obj.GetName(), LabelRun, t.r.config.RunID)
	}
	return nil
}

// update merge-patches the object named in resource with the resource's
// fields (everything but apiVersion, kind, metadata.name and
// metadata.namespace). The run labels cannot be changed.
func (t *testRun) update(ctx context.Context, gvk schema.GroupVersionKind, name string, resource map[string]interface{}) error {
	obj, err := t.get(ctx, gvk, name)
	if err != nil {
		return fmt.Errorf("get %s/%s: %w", gvk.Kind, name, err)
	}
	if err := t.ownedByRun(obj); err != nil {
		return err
	}

	patch := make(map[string]interface{}, len(resource))
	for k, v := range resource {
		if k == "apiVersion" || k == "kind" || k == "metadata" {
			continue
		}
		patch[k] = v
	}
	if meta, ok := resource["metadata"].(map[string]interface{}); ok {
		m := map[string]interface{}{}
		if labels, ok := meta["labels"].(map[string]interface{}); ok {
			for _, reserved := range []string{LabelRun, LabelTest, LabelManagedBy} {
				if _, set := labels[reserved]; set {
					return fmt.Errorf("an update cannot change the label %s", reserved)
				}
			}
			m["labels"] = labels
		}
		if annotations, ok := meta["annotations"]; ok {
			m["annotations"] = annotations
		}
		if len(m) > 0 {
			patch["metadata"] = m
		}
	}
	if len(patch) == 0 {
		return errors.New("an update needs at least one field besides apiVersion, kind and metadata.name")
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode patch: %w", err)
	}
	if err := t.r.config.KubeClient.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data)); err != nil {
		return fmt.Errorf("update %s/%s: %w", gvk.Kind, name, err)
	}
	return nil
}

// delete deletes the object of gvk named name, which must belong to this
// run. It does not wait for the object to be gone (a wait step with
// condition Deleted does). With ignoreMissing, an object that is already
// gone is not an error.
func (t *testRun) delete(ctx context.Context, gvk schema.GroupVersionKind, name string, ignoreMissing bool) error {
	obj, err := t.get(ctx, gvk, name)
	if err != nil {
		if ignoreMissing && apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %s/%s: %w", gvk.Kind, name, err)
	}
	if err := t.ownedByRun(obj); err != nil {
		return err
	}
	uid := obj.GetUID()
	if err := t.r.config.KubeClient.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil {
		if ignoreMissing && apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete %s/%s: %w", gvk.Kind, name, err)
	}
	return nil
}

// validateOnce reads the object and checks every assertion once. It
// reports every assertion that does not hold.
func (t *testRun) validateOnce(ctx context.Context, gvk schema.GroupVersionKind, name string, validations []Validation) error {
	if len(validations) == 0 {
		return nil
	}
	obj, err := t.get(ctx, gvk, name)
	if err != nil {
		return fmt.Errorf("get %s/%s: %w", gvk.Kind, name, err)
	}
	if failures := checkAll(obj, validations); len(failures) > 0 {
		return fmt.Errorf("%s/%s: %s", gvk.Kind, name, strings.Join(failures, "; "))
	}
	return nil
}

// checkAll evaluates every assertion and returns the failures.
func checkAll(obj *unstructured.Unstructured, validations []Validation) []string {
	var failures []string
	for _, v := range validations {
		if err := evaluate(obj.Object, v); err != nil {
			failures = append(failures, err.Error())
		}
	}
	return failures
}

// waitFor polls the object of gvk named name until the condition cond (if
// any) holds and every assertion passes, or until ctx ends. Waiting for any
// condition but Failed stops at once, as a failure, when the object's
// status.phase is Failed. An object that does not exist yet is waited for
// (it may be created by the operator, like a clone's target), except that
// for Deleted its absence is the success.
func (t *testRun) waitFor(ctx context.Context, gvk schema.GroupVersionKind, name, cond string,
	validations []Validation) error {
	start := time.Now()
	last := "not read yet"
	err := wait.PollUntilContextCancel(ctx, t.pollInterval(), true, func(ctx context.Context) (bool, error) {
		obj, err := t.get(ctx, gvk, name)
		if apierrors.IsNotFound(err) {
			last = "does not exist"
			return cond == CondDeleted, nil
		}
		if err != nil {
			last = fmt.Sprintf("read failed: %v", err)
			return false, nil // transient API errors are retried until the timeout
		}
		if cond == CondDeleted {
			last = "still exists"
			if obj.GetDeletionTimestamp() != nil {
				last = "still exists (being deleted"
				if fins := obj.GetFinalizers(); len(fins) > 0 {
					last += ", finalizers " + strings.Join(fins, ",")
				}
				last += ")"
			}
			return false, nil
		}
		if cond != CondFailed {
			if failed := failedPhase(obj); failed != "" {
				return false, fmt.Errorf("%s/%s failed: %s", gvk.Kind, name, failed)
			}
		}
		var parts []string
		met := true
		if cond != "" {
			ok, desc := conditionMet(obj, cond)
			met = ok
			parts = append(parts, desc)
		}
		if failures := checkAll(obj, validations); len(failures) > 0 {
			met = false
			parts = append(parts, failures...)
		}
		last = strings.Join(parts, "; ")
		return met, nil
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		what := cond
		if what == "" {
			what = "the assertions"
		}
		return fmt.Errorf("timed out after %v waiting for %s on %s/%s; last seen: %s",
			time.Since(start).Round(time.Second), what, gvk.Kind, name, last)
	}
	return err
}

// trackedNames returns the objects the test created, as Kind/name.
func (t *testRun) trackedNames() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.tracked))
	for _, o := range t.tracked {
		names = append(names, o.String())
	}
	return names
}

// cleanup runs the specification's cleanup steps, then deletes — and waits
// until they are gone — the objects labelled with this run and test that
// the runner did not create (objects created on the test's behalf), then the
// objects the test created, newest first. It runs to the end even when ctx
// is cancelled (an interrupted run still cleans up), bounded by the cleanup
// timeout.
func (t *testRun) cleanup(ctx context.Context) []StepResult {
	timeout := t.r.config.CleanupTimeout
	if timeout <= 0 {
		timeout = DefaultCleanupTimeout
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	var results []StepResult
	for _, step := range t.test.Cleanup {
		results = append(results, t.runStep(cctx, step, true))
	}

	t.mu.Lock()
	tracked := append([]trackedObject{}, t.tracked...)
	t.mu.Unlock()

	// Objects created on the test's behalf, found by label.
	trackedUIDs := map[types.UID]bool{}
	kinds := append([]string{}, sweepKinds...)
	seenKind := map[string]bool{}
	for _, k := range kinds {
		seenKind[k] = true
	}
	for _, o := range tracked {
		trackedUIDs[o.uid] = true
		if !seenKind[o.gvk.Kind] {
			seenKind[o.gvk.Kind] = true
			kinds = append(kinds, o.gvk.Kind)
		}
	}
	var derived []trackedObject
	for _, kind := range kinds {
		found, res := t.listRunObjects(cctx, kind)
		if res != nil {
			results = append(results, *res)
		}
		for _, o := range found {
			if !trackedUIDs[o.uid] {
				derived = append(derived, o)
			}
		}
	}
	for _, o := range derived {
		results = append(results, t.deleteAndWait(cctx, o, "delete "+o.String()+" (labelled with this run)"))
	}

	// Objects the test created, newest first.
	for i := len(tracked) - 1; i >= 0; i-- {
		results = append(results, t.deleteAndWait(cctx, tracked[i], "delete "+tracked[i].String()))
	}
	return results
}

// listRunObjects lists the objects of kind labelled with this run and test.
// A list failure is returned as a failed cleanup result.
func (t *testRun) listRunObjects(ctx context.Context, kind string) ([]trackedObject, *StepResult) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "infra.virtrigaud.io", Version: "v1beta1", Kind: kind + "List"})
	err := t.r.config.KubeClient.List(ctx, list, client.InNamespace(t.r.config.Namespace),
		client.MatchingLabels{LabelRun: t.r.config.RunID, LabelTest: t.test.Name})
	if err != nil {
		return nil, &StepResult{
			Name: "list " + kind + " labelled with this run", Status: StatusFailed,
			Error: fmt.Sprintf("list %s: %v", kind, err),
		}
	}
	out := make([]trackedObject, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, trackedObject{gvk: item.GroupVersionKind(), name: item.GetName(), uid: item.GetUID()})
	}
	return out, nil
}

// deleteAndWait deletes o (only that incarnation: the delete carries its
// UID as a precondition) and waits until it is gone.
func (t *testRun) deleteAndWait(ctx context.Context, o trackedObject, stepName string) StepResult {
	start := time.Now()
	res := StepResult{Name: stepName, Status: StatusPassed}
	u := t.newObject(o.gvk, o.name)
	uid := o.uid
	err := t.r.config.KubeClient.Delete(ctx, u, client.Preconditions{UID: &uid})
	switch {
	case apierrors.IsNotFound(err):
		// already gone
	case apierrors.IsConflict(err):
		// The name now belongs to another object (precondition failed): ours
		// is gone; the other one is not ours to delete.
	case err != nil:
		res.Status = StatusFailed
		res.Error = fmt.Sprintf("delete %s: %v", o, err)
	default:
		werr := wait.PollUntilContextCancel(ctx, t.pollInterval(), true, func(ctx context.Context) (bool, error) {
			cur, err := t.get(ctx, o.gvk, o.name)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, nil
			}
			return cur.GetUID() != o.uid, nil
		})
		if werr != nil {
			res.Status = StatusFailed
			res.Error = fmt.Sprintf("%s still exists when the cleanup timeout ended: %v", o, werr)
		}
	}
	res.Duration = time.Since(start)
	return res
}
