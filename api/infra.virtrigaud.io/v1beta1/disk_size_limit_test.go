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

package v1beta1

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseMaxDiskSize(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string // "" = refused
	}{
		{"500Gi", "500Gi"},
		{"1Gi", "1Gi"},
		{"2Ti", "2Ti"},
		{"1073741824", "1Gi"},
		{"", ""},
		{"0", ""},
		{"-1Gi", ""},
		{"500", ""},   // 500 bytes: a missing unit
		{"1023Mi", ""}, // below 1Gi
		{"lots", ""},
		{"none", ""},
	} {
		got, err := ParseMaxDiskSize(tc.value)
		if tc.want == "" {
			if err == nil {
				t.Errorf("ParseMaxDiskSize(%q) = %s, want an error", tc.value, got.String())
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMaxDiskSize(%q): %v", tc.value, err)
			continue
		}
		want := resource.MustParse(tc.want)
		if got.Cmp(want) != 0 {
			t.Errorf("ParseMaxDiskSize(%q) = %s, want %s", tc.value, got.String(), tc.want)
		}
	}
}

// TestDiskSizeGiBQuantity: GiB is binary — a decimal "G" limit is smaller
// than the same number of GiB.
func TestDiskSizeGiBQuantity(t *testing.T) {
	q := DiskSizeGiBQuantity(500)
	if q.Cmp(resource.MustParse("500Gi")) != 0 {
		t.Fatalf("500 GiB = %s, want 500Gi", q.String())
	}
	if q.Cmp(resource.MustParse("500G")) <= 0 {
		t.Fatalf("500 GiB must exceed 500G (decimal)")
	}
	if big := DiskSizeGiBQuantity(65536); big.Cmp(resource.MustParse("64Ti")) != 0 {
		t.Fatalf("65536 GiB = %s, want 64Ti", big.String())
	}
}

func providerWithMaxDisk(value *string) *Provider {
	p := &Provider{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if value != nil {
		p.Annotations = map[string]string{ProviderMaxDiskSizeAnnotation: *value}
	}
	return p
}

func strp(s string) *string { return &s }

func TestEffectiveMaxDiskSize(t *testing.T) {
	q := func(s string) *resource.Quantity { v := resource.MustParse(s); return &v }
	for _, tc := range []struct {
		name       string
		ceiling    *resource.Quantity
		annotation *string
		want       string // "" = no limit
		wantErr    bool
	}{
		{name: "none", want: ""},
		{name: "ceiling only", ceiling: q("500Gi"), want: "500Gi"},
		{name: "annotation only", annotation: strp("200Gi"), want: "200Gi"},
		{name: "annotation lowers the ceiling", ceiling: q("500Gi"), annotation: strp("200Gi"), want: "200Gi"},
		{name: "annotation cannot raise the ceiling", ceiling: q("500Gi"), annotation: strp("2Ti"), want: "500Gi"},
		{name: "invalid annotation fails closed", ceiling: q("500Gi"), annotation: strp("lots"), wantErr: true},
		{name: "empty annotation fails closed", annotation: strp(""), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveMaxDiskSize(tc.ceiling, providerWithMaxDisk(tc.annotation))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %s, want no limit", got.String())
				}
				return
			}
			if got == nil || got.Cmp(resource.MustParse(tc.want)) != 0 {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
	if got, err := EffectiveMaxDiskSize(q("1Ti"), nil); err != nil || got.Cmp(resource.MustParse("1Ti")) != 0 {
		t.Fatalf("nil provider: got %v, %v", got, err)
	}
}

func TestFirstDiskOverLimit(t *testing.T) {
	limit := resource.MustParse("100Gi")
	disks := SpecDiskSizeRequests([]DiskSpec{{Name: "a", SizeGiB: 100}, {Name: "b", SizeGiB: 101}, {Name: "c", SizeGiB: 500}})
	d, over := FirstDiskOverLimit(disks, &limit)
	if !over || d.Field != "spec.disks[b].sizeGiB" {
		t.Fatalf("got %+v %v, want disk b (a disk AT the limit is allowed)", d, over)
	}
	if _, over := FirstDiskOverLimit(disks, nil); over {
		t.Fatal("no limit refuses nothing")
	}
	class := &VMClass{ObjectMeta: metav1.ObjectMeta{Name: "big"}, Spec: VMClassSpec{DiskDefaults: &DiskDefaults{Size: resource.MustParse("100.5Gi")}}}
	req, ok := ClassDiskSizeRequest(class)
	if !ok || req.Field != "VMClass big spec.diskDefaults.size" {
		t.Fatalf("class request: %+v %v", req, ok)
	}
	if _, over := FirstDiskOverLimit([]DiskSizeRequest{req}, &limit); !over {
		t.Fatal("100.5Gi is above a 100Gi limit")
	}
	if _, ok := ClassDiskSizeRequest(&VMClass{}); ok {
		t.Fatal("a class with no diskDefaults asks for no size")
	}
}

func vmWithDisks(sizes map[string]int32, order ...string) *VirtualMachine {
	vm := &VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"}}
	for _, n := range order {
		vm.Spec.Disks = append(vm.Spec.Disks, DiskSpec{Name: n, SizeGiB: sizes[n]})
	}
	return vm
}

func TestVirtualMachineDiskSizeValidator(t *testing.T) {
	v := &VirtualMachineDiskSizeValidator{MaxDiskSize: resource.MustParse("100Gi")}
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		old     *VirtualMachine // nil = create
		new     *VirtualMachine
		wantErr string // "" = admitted; else the field path in the error
	}{
		{name: "create within the limit", new: vmWithDisks(map[string]int32{"a": 100}, "a")},
		{name: "create with no disks", new: vmWithDisks(nil)},
		{name: "create above the limit", new: vmWithDisks(map[string]int32{"a": 10, "b": 101}, "a", "b"),
			wantErr: "spec.disks[1].sizeGiB"},
		{name: "grow within the limit", old: vmWithDisks(map[string]int32{"a": 50}, "a"),
			new: vmWithDisks(map[string]int32{"a": 100}, "a")},
		{name: "grow beyond the limit", old: vmWithDisks(map[string]int32{"a": 50}, "a"),
			new: vmWithDisks(map[string]int32{"a": 101}, "a"), wantErr: "spec.disks[0].sizeGiB"},
		{name: "a disk already above the limit is kept", old: vmWithDisks(map[string]int32{"a": 500}, "a"),
			new: vmWithDisks(map[string]int32{"a": 500}, "a")},
		{name: "a disk already above the limit may shrink in spec", old: vmWithDisks(map[string]int32{"a": 500}, "a"),
			new: vmWithDisks(map[string]int32{"a": 400}, "a")},
		{name: "a disk already above the limit may not grow", old: vmWithDisks(map[string]int32{"a": 500}, "a"),
			new: vmWithDisks(map[string]int32{"a": 501}, "a"), wantErr: "spec.disks[0].sizeGiB"},
		{name: "a new disk above the limit", old: vmWithDisks(map[string]int32{"a": 500}, "a"),
			new: vmWithDisks(map[string]int32{"a": 500, "b": 200}, "a", "b"), wantErr: "spec.disks[1].sizeGiB"},
		{name: "renaming an over-limit disk is a new disk", old: vmWithDisks(map[string]int32{"a": 500}, "a"),
			new: vmWithDisks(map[string]int32{"z": 500}, "z"), wantErr: "spec.disks[0].sizeGiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.old == nil {
				_, err = v.ValidateCreate(ctx, tc.new)
			} else {
				_, err = v.ValidateUpdate(ctx, tc.old, tc.new)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want admitted, got %v", err)
				}
				return
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.wantErr) ||
				!strings.Contains(err.Error(), "disk-size limit 100Gi") {
				t.Fatalf("want an Invalid error naming %s and the limit, got %v", tc.wantErr, err)
			}
		})
	}
	if _, err := v.ValidateDelete(ctx, vmWithDisks(map[string]int32{"a": 500}, "a")); err != nil {
		t.Fatalf("delete is always admitted: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, &VMClass{}); err == nil {
		t.Fatal("a wrong type is an error")
	}
}

func classWithDisk(size string) *VMClass {
	c := &VMClass{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "team-a"}}
	if size != "" {
		c.Spec.DiskDefaults = &DiskDefaults{Size: resource.MustParse(size)}
	}
	return c
}

func TestVMClassDiskSizeValidator(t *testing.T) {
	v := &VMClassDiskSizeValidator{MaxDiskSize: resource.MustParse("100Gi")}
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		old     *VMClass
		new     *VMClass
		refused bool
	}{
		{name: "create within the limit", new: classWithDisk("100Gi")},
		{name: "create without diskDefaults", new: classWithDisk("")},
		{name: "create above the limit", new: classWithDisk("101Gi"), refused: true},
		{name: "create with a decimal size above the limit", new: classWithDisk("108G"), refused: true},
		{name: "grow beyond the limit", old: classWithDisk("40Gi"), new: classWithDisk("200Gi"), refused: true},
		{name: "add diskDefaults above the limit", old: classWithDisk(""), new: classWithDisk("200Gi"), refused: true},
		{name: "a class already above the limit is kept", old: classWithDisk("200Gi"), new: classWithDisk("200Gi")},
		{name: "a class already above the limit may be lowered", old: classWithDisk("200Gi"), new: classWithDisk("150Gi")},
		{name: "a class already above the limit may not grow", old: classWithDisk("200Gi"), new: classWithDisk("300Gi"), refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.old == nil {
				_, err = v.ValidateCreate(ctx, tc.new)
			} else {
				_, err = v.ValidateUpdate(ctx, tc.old, tc.new)
			}
			if !tc.refused {
				if err != nil {
					t.Fatalf("want admitted, got %v", err)
				}
				return
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "spec.diskDefaults.size") {
				t.Fatalf("want an Invalid error on spec.diskDefaults.size, got %v", err)
			}
		})
	}
}

func TestProviderWebhook_MaxDiskSizeAnnotation(t *testing.T) {
	v := &ProviderCustomValidator{}
	ctx := context.Background()
	withAnn := func(value *string) *Provider {
		p := newProviderForTopology("p", ProviderTypeLibvirt, "")
		if value != nil {
			p.Annotations = map[string]string{ProviderMaxDiskSizeAnnotation: *value}
		}
		return p
	}
	for _, tc := range []struct {
		name    string
		old     *string
		new     *string
		update  bool
		refused bool
	}{
		{name: "create without", new: nil},
		{name: "create valid", new: strp("500Gi")},
		{name: "create invalid", new: strp("lots"), refused: true},
		{name: "create below 1Gi", new: strp("500"), refused: true},
		{name: "update to invalid", update: true, old: strp("500Gi"), new: strp("0"), refused: true},
		{name: "update keeping a stored invalid value", update: true, old: strp("lots"), new: strp("lots")},
		{name: "update valid", update: true, old: nil, new: strp("1Ti")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.update {
				_, err = v.ValidateUpdate(ctx, withAnn(tc.old), withAnn(tc.new))
			} else {
				_, err = v.ValidateCreate(ctx, withAnn(tc.new))
			}
			if tc.refused != (err != nil) {
				t.Fatalf("refused=%v, got %v", tc.refused, err)
			}
			if tc.refused && !strings.Contains(err.Error(), ProviderMaxDiskSizeAnnotation) {
				t.Fatalf("the error names the annotation: %v", err)
			}
		})
	}
}
