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

package imageartifact

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// digestCoveredFields are the ImageSource leaf fields (JSON paths) the source
// digest covers (ADR-0009 D2, Q7: what the image is, and where it is placed).
var digestCoveredFields = []string{
	"vsphere.templateName",
	"vsphere.contentLibrary.library",
	"vsphere.contentLibrary.item",
	"vsphere.contentLibrary.version",
	"vsphere.ovaURL", // userinfo stripped (TestSourceDigestExcludesURLUserinfo)
	"vsphere.checksum",
	"vsphere.checksumType",
	"libvirt.path",
	"libvirt.url", // userinfo stripped
	"libvirt.format",
	"libvirt.checksum",
	"libvirt.checksumType",
	"libvirt.storagePool",
	"http.url", // userinfo stripped
	"http.checksum",
	"http.checksumType",
	"registry.image",
	"registry.format",
	"dataVolume.name",
	"dataVolume.namespace",
	"proxmox.templateID",
	"proxmox.templateName",
	"proxmox.storage",
	"proxmox.node",
	"proxmox.format",
	"proxmox.fullClone",
}

// digestExcludedPrefixes are the ImageSource fields (and everything under
// them) the source digest excludes: how, or by whom, the bytes are fetched.
var digestExcludedPrefixes = []string{
	"http.timeout",
	"http.headers",
	"http.authentication",
	"registry.pullSecretRef",
	"vsphere.providerRef",
}

// sourceLeaf is one leaf field of ImageSource: its JSON path and the field
// indices that reach it.
type sourceLeaf struct {
	path  string
	index []int
}

// durationType is treated as a leaf (it encodes as one JSON string).
var durationType = reflect.TypeOf(metav1.Duration{})

// sourceLeaves returns every leaf field under t (a struct type), recursing
// into struct and pointer-to-struct fields.
func sourceLeaves(t reflect.Type, prefix string, index []int) []sourceLeaf {
	var out []sourceLeaf
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			name = f.Name
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		idx := append(append([]int{}, index...), i)
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != durationType {
			out = append(out, sourceLeaves(ft, path, idx)...)
			continue
		}
		out = append(out, sourceLeaf{path: path, index: idx})
	}
	return out
}

// leafValue returns the field at index in v (an addressable struct),
// allocating every nil pointer on the way.
func leafValue(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v
}

// setNonZero sets the leaf field v to a non-zero value of its type.
func setNonZero(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.New(v.Type().Elem()))
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("https://example.com/leaf")
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(101)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf("v"))
		v.Set(m)
	case reflect.Struct:
		if v.Type() != durationType {
			t.Fatalf("%s: unexpected struct leaf %s", path, v.Type())
		}
		v.Set(reflect.ValueOf(metav1.Duration{Duration: time.Hour}))
	default:
		t.Fatalf("%s: unsupported leaf kind %s; extend setNonZero", path, v.Kind())
	}
}

// isExcludedField reports whether path is, or is under, an excluded prefix.
func isExcludedField(path string) bool {
	for _, p := range digestExcludedPrefixes {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

// TestSourceDigestFieldClassification walks every leaf field of ImageSource
// and fails when one is neither explicitly covered nor excluded, so a field
// added to the API can never silently join (or stay out of) the digest: whoever
// adds it must decide (ADR-0009 D2: defaulted fields, and any spec.prepare
// field a provider starts honouring, need a deliberate decision). It also
// checks each classification behaviourally: setting a covered field changes
// the digest; setting an excluded one does not.
func TestSourceDigestFieldClassification(t *testing.T) {
	leaves := sourceLeaves(reflect.TypeOf(infravirtrigaudiov1beta1.ImageSource{}), "", nil)
	covered := map[string]bool{}
	for _, f := range digestCoveredFields {
		covered[f] = true
	}

	seen := map[string]bool{}
	excludedSeen := map[string]bool{}
	for _, leaf := range leaves {
		seen[leaf.path] = true
		excluded := isExcludedField(leaf.path)
		for _, p := range digestExcludedPrefixes {
			if leaf.path == p || strings.HasPrefix(leaf.path, p+".") {
				excludedSeen[p] = true
			}
		}
		switch {
		case covered[leaf.path] && excluded:
			t.Errorf("%s is listed as both covered and excluded", leaf.path)
			continue
		case !covered[leaf.path] && !excluded:
			t.Errorf("ImageSource field %s is not classified: add it to digestCoveredFields or "+
				"digestExcludedPrefixes (and to canonicalSource) after deciding whether it changes "+
				"which image is prepared or where (ADR-0009 D2)", leaf.path)
			continue
		}

		t.Run(leaf.path, func(t *testing.T) {
			var base infravirtrigaudiov1beta1.ImageSource
			leafValue(reflect.ValueOf(&base).Elem(), leaf.index) // allocate the parents only
			var set infravirtrigaudiov1beta1.ImageSource
			setNonZero(t, leaf.path, leafValue(reflect.ValueOf(&set).Elem(), leaf.index))

			before, after := mustDigest(t, base), mustDigest(t, set)
			if excluded {
				if before != after {
					t.Errorf("excluded field %s changes the digest", leaf.path)
				}
			} else if before == after {
				t.Errorf("covered field %s does not change the digest", leaf.path)
			}
		})
	}

	var stale []string
	for _, f := range digestCoveredFields {
		if !seen[f] {
			stale = append(stale, f)
		}
	}
	for _, p := range digestExcludedPrefixes {
		if !excludedSeen[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("classified fields that no longer exist in ImageSource: %v", stale)
	}
}
