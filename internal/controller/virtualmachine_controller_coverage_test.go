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

package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// ─── stubResolver ─────────────────────────────────────────────────────────────

// stubResolver implements ProviderResolver for unit tests.
type stubResolver struct {
	provider contracts.Provider
	err      error
}

func (s *stubResolver) GetProvider(_ context.Context, _ *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	return s.provider, s.err
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// coverageTestScheme builds a Scheme with both core k8s and virtrigaud types.
func coverageTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := infravirtrigaudiov1beta1.AddToScheme(s); err != nil {
		t.Fatalf("add virtrigaud scheme: %v", err)
	}
	return s
}

// newTestReconciler builds a reconciler backed by a fake k8s client.
func newTestReconciler(s *runtime.Scheme, resolver ProviderResolver, objs ...client.Object) *VirtualMachineReconciler {
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		Build()
	return &VirtualMachineReconciler{
		Client:         fc,
		Scheme:         s,
		RemoteResolver: resolver,
	}
}

// testProvider returns a stub provider whose IsTaskComplete and Reconfigure are controllable.
func testProvider(isTaskDone bool, isTaskErr error, reconfigureTaskRef string, reconfigureErr error) *stubProvider {
	return &stubProvider{
		IsTaskCompleteFn: func(_ context.Context, _ string) (bool, error) {
			return isTaskDone, isTaskErr
		},
		ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
			return reconfigureTaskRef, reconfigureErr
		},
	}
}

// fakeProvider returns a no-op stub (validate nil, describe exists+poweredOn).
type fakeDescribeProvider struct {
	stubProvider
	DescribeFn func(ctx context.Context, id string) (contracts.DescribeResponse, error)
}

func (f *fakeDescribeProvider) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	if f.DescribeFn != nil {
		return f.DescribeFn(ctx, id)
	}
	return contracts.DescribeResponse{}, nil
}

// providerAndClass returns ready-to-use Provider + VMClass k8s objects.
// The Provider has a non-nil Runtime status so reconcileVM doesn't nil-deref it.
func providerAndClass(ns string) (*infravirtrigaudiov1beta1.Provider, *infravirtrigaudiov1beta1.VMClass) {
	prov := &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "test-prov", Namespace: ns},
		Status: infravirtrigaudiov1beta1.ProviderStatus{
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeStatus{
				Phase:    "Running",
				Endpoint: "localhost:50051",
			},
		},
	}
	class := &infravirtrigaudiov1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-class", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMClassSpec{
			CPU:    4,
			Memory: resource.MustParse("8Gi"),
		},
	}
	return prov, class
}

// baseVM returns a minimal VirtualMachine pointing at the shared provider/class.
func baseVM(ns string) *infravirtrigaudiov1beta1.VirtualMachine {
	return &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vm", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "test-prov"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "test-class"},
		},
	}
}

// ─── buildCreateRequest ───────────────────────────────────────────────────────

func TestBuildCreateRequest_NoNetworks(t *testing.T) {
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 4, Memory: resource.MustParse("8Gi")},
	}

	req := r.buildCreateRequest(vm, vmClass, nil, nil)

	if len(req.Networks) != 0 {
		t.Errorf("expected 0 networks, got %d", len(req.Networks))
	}
	if req.Class.CPU != 4 {
		t.Errorf("expected CPU 4, got %d", req.Class.CPU)
	}
	if req.Class.MemoryMiB != 8192 {
		t.Errorf("expected MemoryMiB 8192, got %d", req.Class.MemoryMiB)
	}
}

func TestBuildCreateRequest_NilNetworkRef_ElseBranch(t *testing.T) {
	// When NetworkRef is nil the else-if branch executes and attachment is still appended.
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: nil, IPAddress: "10.0.0.1", Prefix: 24, Gateway: "10.0.0.254"},
	}
	networks := []*infravirtrigaudiov1beta1.VMNetworkAttachment{nil}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req := r.buildCreateRequest(vm, vmClass, nil, networks)

	if len(req.Networks) != 1 {
		t.Fatalf("expected 1 network attachment, got %d", len(req.Networks))
	}
	if req.Networks[0].StaticIP != "10.0.0.1" {
		t.Errorf("expected StaticIP '10.0.0.1', got '%s'", req.Networks[0].StaticIP)
	}
	if req.Networks[0].Prefix != 24 {
		t.Errorf("expected Prefix 24, got %d", req.Networks[0].Prefix)
	}
	if req.Networks[0].Gateway != "10.0.0.254" {
		t.Errorf("expected Gateway '10.0.0.254', got '%s'", req.Networks[0].Gateway)
	}
	if req.Networks[0].NetworkName != "" {
		t.Error("expected empty NetworkName when NetworkRef is nil")
	}
	if req.Networks[0].PCISlotNumber != nil {
		t.Error("expected nil PCISlotNumber when NetworkRef is nil")
	}
}

func TestBuildCreateRequest_VSphereNetwork_NilPCISlot(t *testing.T) {
	// NetworkRef != nil, VSphere config present, PCISlotNumber nil → not set.
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "net1"}},
	}
	netAttach := &infravirtrigaudiov1beta1.VMNetworkAttachment{
		Spec: infravirtrigaudiov1beta1.VMNetworkAttachmentSpec{
			Network: infravirtrigaudiov1beta1.NetworkConfig{
				VSphere: &infravirtrigaudiov1beta1.VSphereNetworkConfig{
					Portgroup:     "VM Network",
					PCISlotNumber: nil,
				},
			},
		},
	}
	networks := []*infravirtrigaudiov1beta1.VMNetworkAttachment{netAttach}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req := r.buildCreateRequest(vm, vmClass, nil, networks)

	if len(req.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(req.Networks))
	}
	if req.Networks[0].NetworkName != "VM Network" {
		t.Errorf("expected NetworkName 'VM Network', got '%s'", req.Networks[0].NetworkName)
	}
	if req.Networks[0].PCISlotNumber != nil {
		t.Error("expected nil PCISlotNumber when VSphere.PCISlotNumber is nil")
	}
}

func TestBuildCreateRequest_VSphereNetwork_WithPCISlot(t *testing.T) {
	// NetworkRef != nil, VSphere config present, PCISlotNumber set → forwarded.
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	pciSlot := int32(192)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "net1"}, IPAddress: "10.0.0.5"},
	}
	netAttach := &infravirtrigaudiov1beta1.VMNetworkAttachment{
		Spec: infravirtrigaudiov1beta1.VMNetworkAttachmentSpec{
			Network: infravirtrigaudiov1beta1.NetworkConfig{
				VSphere: &infravirtrigaudiov1beta1.VSphereNetworkConfig{
					Portgroup:     "VM Network",
					PCISlotNumber: &pciSlot,
				},
			},
		},
	}
	networks := []*infravirtrigaudiov1beta1.VMNetworkAttachment{netAttach}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req := r.buildCreateRequest(vm, vmClass, nil, networks)

	if len(req.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(req.Networks))
	}
	if req.Networks[0].PCISlotNumber == nil {
		t.Fatal("expected non-nil PCISlotNumber")
	}
	if *req.Networks[0].PCISlotNumber != 192 {
		t.Errorf("expected PCISlotNumber 192, got %d", *req.Networks[0].PCISlotNumber)
	}
	if req.Networks[0].StaticIP != "10.0.0.5" {
		t.Errorf("expected StaticIP '10.0.0.5', got '%s'", req.Networks[0].StaticIP)
	}
}

func TestBuildCreateRequest_NetworkRefWithNilNetworksEntry(t *testing.T) {
	// NetworkRef != nil but the networks slice entry is nil (guard condition).
	// Attachment is still appended but without provider-specific details.
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "net1"}, IPAddress: "10.0.0.2"},
	}
	networks := []*infravirtrigaudiov1beta1.VMNetworkAttachment{nil}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	req := r.buildCreateRequest(vm, vmClass, nil, networks)

	if len(req.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(req.Networks))
	}
	if req.Networks[0].NetworkName != "" {
		t.Error("expected empty NetworkName when networks[i] is nil")
	}
	if req.Networks[0].StaticIP != "10.0.0.2" {
		t.Errorf("expected StaticIP '10.0.0.2', got '%s'", req.Networks[0].StaticIP)
	}
}

func TestBuildCreateRequest_VMImageVSphere(t *testing.T) {
	// VMImage with VSphere source sets TemplateName.
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := baseVM("default")
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}
	vmImage := &infravirtrigaudiov1beta1.VMImage{
		Spec: infravirtrigaudiov1beta1.VMImageSpec{
			Source: infravirtrigaudiov1beta1.ImageSource{
				VSphere: &infravirtrigaudiov1beta1.VSphereImageSource{
					TemplateName: "rhel9-template",
					Checksum:     "abc123",
					ChecksumType: "sha256",
				},
			},
		},
	}

	req := r.buildCreateRequest(vm, vmClass, vmImage, nil)

	if req.Image.TemplateName != "rhel9-template" {
		t.Errorf("expected TemplateName 'rhel9-template', got '%s'", req.Image.TemplateName)
	}
	if req.Image.Checksum != "abc123" {
		t.Errorf("expected Checksum 'abc123', got '%s'", req.Image.Checksum)
	}
	if req.Image.ChecksumType != "sha256" {
		t.Errorf("expected ChecksumType 'sha256', got '%s'", req.Image.ChecksumType)
	}
}

// ─── getDependencies ──────────────────────────────────────────────────────────

func TestGetDependencies_NilNetworkRef_AppendsNil(t *testing.T) {
	s := coverageTestScheme(t)
	prov, class := providerAndClass("default")
	r := newTestReconciler(s, nil, prov, class)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: nil},
	}

	_, _, _, networks, err := r.getDependencies(context.Background(), vm)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 network slot, got %d", len(networks))
	}
	if networks[0] != nil {
		t.Error("expected networks[0] to be nil for nil NetworkRef")
	}
}

func TestGetDependencies_NilNetworkRef_MultipleNetworks(t *testing.T) {
	// Mixed: first has NetworkRef=nil, second has NetworkRef pointing to an object.
	s := coverageTestScheme(t)
	prov, class := providerAndClass("default")
	netAttach := &infravirtrigaudiov1beta1.VMNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "net1", Namespace: "default"},
	}
	r := newTestReconciler(s, nil, prov, class, netAttach)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: nil},
		{Name: "eth1", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "net1"}},
	}

	_, _, _, networks, err := r.getDependencies(context.Background(), vm)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(networks) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(networks))
	}
	if networks[0] != nil {
		t.Error("expected networks[0] nil for nil NetworkRef")
	}
	if networks[1] == nil || networks[1].Name != "net1" {
		t.Error("expected networks[1] to be the VMNetworkAttachment 'net1'")
	}
}

func TestGetDependencies_NetworkRefFound(t *testing.T) {
	s := coverageTestScheme(t)
	prov, class := providerAndClass("default")
	netAttach := &infravirtrigaudiov1beta1.VMNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "net1", Namespace: "default"},
	}
	r := newTestReconciler(s, nil, prov, class, netAttach)
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "net1"}},
	}

	_, _, _, networks, err := r.getDependencies(context.Background(), vm)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(networks))
	}
	if networks[0] == nil || networks[0].Name != "net1" {
		t.Error("expected networks[0] to be VMNetworkAttachment 'net1'")
	}
}

func TestGetDependencies_NetworkRefNotFound_ReturnsError(t *testing.T) {
	s := coverageTestScheme(t)
	prov, class := providerAndClass("default")
	r := newTestReconciler(s, nil, prov, class) // no VMNetworkAttachment registered
	vm := baseVM("default")
	vm.Spec.Networks = []infravirtrigaudiov1beta1.VMNetworkRef{
		{Name: "eth0", NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "missing-net"}},
	}

	_, _, _, _, err := r.getDependencies(context.Background(), vm)

	if err == nil {
		t.Fatal("expected error for missing VMNetworkAttachment, got nil")
	}
	if !strings.Contains(err.Error(), "missing-net") {
		t.Errorf("expected error to reference 'missing-net', got: %v", err)
	}
}

// ─── reconcileVM — ReconfigureTaskRef block ───────────────────────────────────

// setupReconcileVMReconciler builds a reconciler with Provider+VMClass and a stub provider.
func setupReconcileVMReconciler(t *testing.T, prov contracts.Provider) (*VirtualMachineReconciler, *infravirtrigaudiov1beta1.VMClass) {
	t.Helper()
	s := coverageTestScheme(t)
	k8sProv, class := providerAndClass("default")
	resolver := &stubResolver{provider: prov}
	r := newTestReconciler(s, resolver, k8sProv, class)
	return r, class
}

// baseVMWithReconfigureTask returns a VM with ReconfigureTaskRef set and an existing provider ID.
func baseVMWithReconfigureTask(taskRef string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := baseVM("default")
	vm.Status.ReconfigureTaskRef = taskRef
	vm.Status.ID = "vm-abc"
	return vm
}

func TestReconcileVM_ReconfigureTaskRef_IsTaskCompleteError(t *testing.T) {
	prov := testProvider(false, errTest("provider connection failed"), "", nil)
	r, _ := setupReconcileVMReconciler(t, prov)
	vm := baseVMWithReconfigureTask("task-999")

	result, err := r.reconcileVM(context.Background(), vm)

	if err != nil {
		t.Fatalf("expected nil error (controller absorbs provider errors), got: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter to be set on IsTaskComplete error")
	}
	// Condition should reflect provider error
	found := false
	for _, c := range vm.Status.Conditions {
		if c.Type == "Reconfiguring" && c.Reason == "ProviderError" {
			found = true
		}
	}
	if !found {
		t.Error("expected Reconfiguring=False/ProviderError condition after IsTaskComplete error")
	}
}

func TestReconcileVM_ReconfigureTaskRef_TaskInProgress(t *testing.T) {
	prov := testProvider(false, nil, "", nil) // done=false, no error
	r, _ := setupReconcileVMReconciler(t, prov)
	vm := baseVMWithReconfigureTask("task-999")

	result, err := r.reconcileVM(context.Background(), vm)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter set while task is in progress")
	}
	// ReconfigureTaskRef must NOT be cleared
	if vm.Status.ReconfigureTaskRef != "task-999" {
		t.Errorf("expected ReconfigureTaskRef 'task-999', got '%s'", vm.Status.ReconfigureTaskRef)
	}
	// Condition should reflect in-progress
	found := false
	for _, c := range vm.Status.Conditions {
		if c.Type == "Reconfiguring" && string(c.Status) == "True" {
			found = true
		}
	}
	if !found {
		t.Error("expected Reconfiguring=True condition while task is in progress")
	}
}

func TestReconcileVM_ReconfigureTaskRef_TaskComplete_ClearsRef(t *testing.T) {
	prov := testProvider(true, nil, "", nil) // done=true
	r, class := setupReconcileVMReconciler(t, prov)

	// Set CurrentResources to match VMClass so needsReconfigure returns false after task completes.
	cpu := class.Spec.CPU
	memMiB := class.Spec.Memory.Value() / (1024 * 1024)
	vm := baseVMWithReconfigureTask("task-999")
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{
		CPU:       &cpu,
		MemoryMiB: &memMiB,
	}

	// After the ReconfigureTaskRef block completes, reconcileVM continues. Since vm.Status.ID
	// is "vm-abc" and Describe returns {Exists:false}, it calls createVM which fails due to
	// no ImageRef — but the assertions below verify the block already ran correctly.
	r.reconcileVM(context.Background(), vm) //nolint:errcheck

	if vm.Status.ReconfigureTaskRef != "" {
		t.Errorf("expected ReconfigureTaskRef cleared, got '%s'", vm.Status.ReconfigureTaskRef)
	}
}

// ─── reconcileVM — needsReconfigure → reconfigureVM ──────────────────────────

func TestReconcileVM_NeedsReconfigure_TriggersReconfigureVM(t *testing.T) {
	// Arrange: VM has ID (exists), Describe returns poweredOn matching desired, but
	// CurrentResources mismatch triggers needsReconfigure.
	var reconfigureCalled bool
	prov := &fakeDescribeProvider{
		stubProvider: stubProvider{
			ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
				reconfigureCalled = true
				return "task-new", nil
			},
		},
		DescribeFn: func(_ context.Context, _ string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{
				Exists:     true,
				PowerState: "On", // matches default desired "On"
				IPs:        []string{"10.0.0.1"},
			}, nil
		},
	}
	s := coverageTestScheme(t)
	k8sProv, class := providerAndClass("default")
	resolver := &stubResolver{provider: prov}
	r := newTestReconciler(s, resolver, k8sProv, class)

	vm := baseVM("default")
	vm.Status.ID = "vm-xyz"
	// CurrentResources mismatch: old CPU=2, class wants CPU=4
	oldCPU := int32(2)
	oldMem := int64(8192)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{
		CPU:       &oldCPU,
		MemoryMiB: &oldMem,
	}

	r.reconcileVM(context.Background(), vm) //nolint:errcheck

	if !reconfigureCalled {
		t.Error("expected Reconfigure to be called when needsReconfigure returns true")
	}
	if vm.Status.ReconfigureTaskRef != "task-new" {
		t.Errorf("expected ReconfigureTaskRef 'task-new', got '%s'", vm.Status.ReconfigureTaskRef)
	}
}

func TestReconcileVM_NeedsReconfigure_False_DoesNotReconfigure(t *testing.T) {
	// Arrange: CurrentResources matches VMClass — no reconfigure triggered.
	var reconfigureCalled bool
	prov := &fakeDescribeProvider{
		stubProvider: stubProvider{
			ReconfigureFn: func(_ context.Context, _ string, _ contracts.CreateRequest) (string, error) {
				reconfigureCalled = true
				return "", nil
			},
		},
		DescribeFn: func(_ context.Context, _ string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{
				Exists:     true,
				PowerState: "On",
				IPs:        []string{"10.0.0.1"},
			}, nil
		},
	}
	s := coverageTestScheme(t)
	k8sProv, class := providerAndClass("default")
	resolver := &stubResolver{provider: prov}
	r := newTestReconciler(s, resolver, k8sProv, class)

	vm := baseVM("default")
	vm.Status.ID = "vm-xyz"
	// CurrentResources matches VMClass → needsReconfigure = false
	cpu := class.Spec.CPU
	memMiB := class.Spec.Memory.Value() / (1024 * 1024)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{
		CPU:       &cpu,
		MemoryMiB: &memMiB,
	}

	r.reconcileVM(context.Background(), vm) //nolint:errcheck

	if reconfigureCalled {
		t.Error("expected Reconfigure NOT to be called when resources match")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func errTest(msg string) error {
	return testError(msg)
}

type testError string

func (e testError) Error() string { return string(e) }
