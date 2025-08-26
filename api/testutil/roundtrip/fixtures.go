package fixtures

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/api/v1alpha1"
)

// CreateFullVirtualMachineAlpha creates a v1alpha1 VirtualMachine with all fields populated
func CreateFullVirtualMachineAlpha() *v1alpha1.VirtualMachine {
	return &v1alpha1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
		},
		Spec: v1alpha1.VirtualMachineSpec{
			ProviderRef: v1alpha1.ObjectRef{
				Name: "test-provider",
			},
			ClassRef: v1alpha1.ObjectRef{
				Name: "test-class",
			},
			ImageRef: v1alpha1.ObjectRef{
				Name: "test-image",
			},
			Networks: []v1alpha1.VMNetworkRef{
				{
					Name:     "network1",
					StaticIP: "192.168.1.10",
				},
				{
					Name: "network2",
				},
			},
			Disks: []v1alpha1.DiskSpec{
				{
					Name:    "disk1",
					SizeGiB: 100,
					Type:    "thin",
				},
			},
			UserData: &v1alpha1.UserData{
				CloudInit: &v1alpha1.CloudInitSpec{
					Inline: ptr.To("test cloud-init"),
				},
			},
			Placement: &v1alpha1.Placement{
				Cluster:   "test-cluster",
				Datastore: "test-datastore",
				Folder:    "test-folder",
			},
			PowerState: "On",
			Tags: []string{
				"tag1",
				"tag2",
			},
		},
	}
}

// CreateFullVirtualMachineBeta creates a v1beta1 VirtualMachine with all fields populated
func CreateFullVirtualMachineBeta() *v1beta1.VirtualMachine {
	return &v1beta1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-vm",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.TypedLocalObjectReference{
				Name: "test-provider",
			},
			ClassRef: v1beta1.TypedLocalObjectReference{
				Name: "test-class",
			},
			ImageRef: v1beta1.TypedLocalObjectReference{
				Name: "test-image",
			},
			Networks: []v1beta1.VMNetworkAttachment{
				{
					NetworkRef: v1beta1.TypedLocalObjectReference{
						Name: "network1",
					},
					IPAddress:  ptr.To("192.168.1.10"),
					MACAddress: "",
				},
				{
					NetworkRef: v1beta1.TypedLocalObjectReference{
						Name: "network2",
					},
				},
			},
			Storage: v1beta1.VMStorageSpec{
				Disks: []v1beta1.VMDiskSpec{
					{
						Name: "disk1",
						Size: resource.MustParse("100Gi"),
						Type: "thin",
					},
				},
			},
			UserData: &v1beta1.UserData{
				CloudInit: &v1beta1.CloudInitSpec{
					Inline: ptr.To("test cloud-init"),
				},
			},
			Placement: &v1beta1.Placement{
				Cluster:      "test-cluster",
				Host:         "",
				Datastore:    "test-datastore",
				Folder:       "test-folder",
				ResourcePool: "",
			},
			PowerState: v1beta1.PowerStateOn,
			Metadata: &v1beta1.VMMetadata{
				Labels: map[string]string{
					"tag1": "",
					"tag2": "",
				},
			},
		},
	}
}

// CreateFullProviderAlpha creates a v1alpha1 Provider with all fields populated
func CreateFullProviderAlpha() *v1alpha1.Provider {
	return &v1alpha1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: v1alpha1.ProviderSpec{
			Type: "vsphere",
			Connection: v1alpha1.ProviderConnection{
				Endpoint: "vcenter.example.com",
				SecretRef: v1alpha1.ObjectRef{
					Name: "vcenter-creds",
				},
				Insecure: ptr.To(false),
			},
			Runtime: &v1alpha1.ProviderRuntimeSpec{
				Mode:     "remote",
				Image:    "test-image",
				Replicas: ptr.To(int32(1)),
			},
		},
	}
}

// CreateFullProviderBeta creates a v1beta1 Provider with all fields populated
func CreateFullProviderBeta() *v1beta1.Provider {
	return &v1beta1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-provider",
			Namespace: "default",
		},
		Spec: v1beta1.ProviderSpec{
			Type: v1beta1.ProviderTypeVSphere,
			Connection: v1beta1.ProviderConnection{
				Endpoint: "vcenter.example.com",
				Auth: v1beta1.ProviderAuth{
					SecretRef: v1beta1.TypedLocalObjectReference{
						Name: "vcenter-creds",
					},
				},
				TLS: &v1beta1.ProviderTLS{
					Insecure: ptr.To(false),
				},
			},
			Runtime: &v1beta1.ProviderRuntimeSpec{
				Mode:     v1beta1.RuntimeModeRemote,
				Image:    "test-image",
				Replicas: ptr.To(int32(1)),
				Service: &v1beta1.ProviderServiceSpec{
					Type: v1beta1.ServiceTypeClusterIP,
				},
			},
		},
	}
}

// CreateMinimalVirtualMachineAlpha creates a minimal v1alpha1 VirtualMachine
func CreateMinimalVirtualMachineAlpha() *v1alpha1.VirtualMachine {
	return &v1alpha1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1alpha1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "minimal-vm",
			Namespace: "default",
		},
		Spec: v1alpha1.VirtualMachineSpec{
			ProviderRef: v1alpha1.ObjectRef{
				Name: "minimal-provider",
			},
			ClassRef: v1alpha1.ObjectRef{
				Name: "minimal-class",
			},
			ImageRef: v1alpha1.ObjectRef{
				Name: "minimal-image",
			},
		},
	}
}

// CreateMinimalVirtualMachineBeta creates a minimal v1beta1 VirtualMachine
func CreateMinimalVirtualMachineBeta() *v1beta1.VirtualMachine {
	return &v1beta1.VirtualMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infra.virtrigaud.io/v1beta1",
			Kind:       "VirtualMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "minimal-vm",
			Namespace: "default",
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.TypedLocalObjectReference{
				Name: "minimal-provider",
			},
			ClassRef: v1beta1.TypedLocalObjectReference{
				Name: "minimal-class",
			},
			ImageRef: v1beta1.TypedLocalObjectReference{
				Name: "minimal-image",
			},
		},
	}
}
