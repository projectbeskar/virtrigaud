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

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

const (
	// examplesDir is the repository's examples/ tree, relative to this package.
	examplesDir = "../../examples"
	// examplesDryRunNamespace receives every example object. CRD schema and
	// CEL validation cannot see metadata.namespace, so one namespace keeps the
	// dry-run faithful without colliding with other specs' namespaces.
	examplesDryRunNamespace = "examples-dryrun"
)

// exampleObject is one infra.virtrigaud.io document from an examples/ file.
type exampleObject struct {
	source string // "<file>#<document index>"
	obj    *unstructured.Unstructured
}

// loadExampleObjects decodes every YAML document under dir the way kubectl
// does (YAML 1.1, so unquoted On/Off become booleans) and returns the
// infra.virtrigaud.io objects plus one error per file that does not parse.
func loadExampleObjects(dir string) ([]exampleObject, []error, error) {
	var (
		out     []exampleObject
		decErrs []error
	)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for i := 0; ; i++ {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				if !errors.Is(err, io.EOF) {
					// The stream cannot resync after a syntax error; skip the rest of the file.
					decErrs = append(decErrs, fmt.Errorf("%s#%d: %w", path, i, err))
				}
				return nil
			}
			if len(doc) == 0 {
				continue
			}
			obj := &unstructured.Unstructured{Object: doc}
			if obj.GroupVersionKind().Group != infravirtrigaudiov1beta1.GroupVersion.Group {
				continue
			}
			out = append(out, exampleObject{source: fmt.Sprintf("%s#%d", path, i), obj: obj})
		}
	})
	return out, decErrs, err
}

// decodeTyped decodes obj into its registered Go type, as a controller's
// cache would.
func decodeTyped(obj *unstructured.Unstructured) error {
	typed, err := k8sClient.Scheme().New(obj.GroupVersionKind())
	if err != nil {
		return err
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, typed)
}

var _ = Describe("examples/ manifests", func() {
	// Strict field validation also rejects unknown fields, which kubectl apply
	// would drop with only a warning. Admission webhooks are not installed in
	// this envtest, so this covers the CRD OpenAPI schema and CEL rules.
	It("pass the CRD schema on a strict server-side dry-run", func() {
		objs, decErrs, err := loadExampleObjects(examplesDir)
		Expect(err).NotTo(HaveOccurred())
		// Guard against a moved examples/ tree silently checking nothing.
		Expect(len(objs)).To(BeNumerically(">", 50), "expected the examples/ tree to hold many infra.virtrigaud.io objects")

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: examplesDryRunNamespace}}
		if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		failures := make([]string, 0, len(decErrs))
		for _, err := range decErrs {
			failures = append(failures, err.Error())
		}
		for _, ex := range objs {
			namespaced, err := k8sClient.IsObjectNamespaced(ex.obj)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s (%s %s): %v", ex.source, ex.obj.GetKind(), ex.obj.GetName(), err))
				continue
			}
			if namespaced {
				ex.obj.SetNamespace(examplesDryRunNamespace)
			}
			if err := k8sClient.Create(ctx, ex.obj, client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
				failures = append(failures, fmt.Sprintf("%s (%s %s): %v", ex.source, ex.obj.GetKind(), ex.obj.GetName(), err))
				continue
			}
			// The schema types some fields as plain strings (metav1.Duration,
			// for one), so an accepted object can still fail to decode into the
			// Go types the controllers' caches use. Decode the server's answer.
			if err := decodeTyped(ex.obj); err != nil {
				failures = append(failures, fmt.Sprintf("%s (%s %s): decoding into the Go type: %v", ex.source, ex.obj.GetKind(), ex.obj.GetName(), err))
			}
		}
		if len(failures) > 0 {
			Fail(fmt.Sprintf("%d example object(s) are invalid:\n%s", len(failures), strings.Join(failures, "\n")))
		}
	})
})
