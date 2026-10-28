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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// layoutConfigMap stores the experiment layout so every subcommand reads
// the same one.
const (
	layoutConfigMap = "spike1b-layout"
	layoutKey       = "layout.json"
	imageName       = "base"
	mib             = 1024 * 1024
)

// providerLayout is one Provider of the experiment.
type providerLayout struct {
	Name       string        `json:"name"`
	Seed       workload.Spec `json:"seed"`
	HostPrefix string        `json:"hostPrefix"`
	Hosts      int           `json:"hosts"`
}

// layout is the whole experiment.
type layout struct {
	Providers  []providerLayout `json:"providers"`
	Namespaces int              `json:"namespaces"`
	HostCPU    int32            `json:"hostCPU"`
	HostMemMiB int64            `json:"hostMemMiB"`
}

// loadLayout reads the layout ConfigMap.
func loadLayout(ctx context.Context, e *env) (*layout, error) {
	var cm corev1.ConfigMap
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: infraNamespace, Name: layoutConfigMap}, &cm); err != nil {
		return nil, fmt.Errorf("read layout (run setup first): %w", err)
	}
	var l layout
	if err := json.Unmarshal([]byte(cm.Data[layoutKey]), &l); err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}
	return &l, nil
}

// setupCmd creates the experiment's infrastructure objects.
func setupCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	providers := fs.Int("providers", 1, "number of clustered Providers the hosts and seeded VMs are split across")
	hosts := fs.Int("hosts", 100, "total Hosts")
	seed := fs.Int("seed", 1000, "total seeded VMs")
	namespaces := fs.Int("namespaces", 10, "tenant namespaces")
	burstHeadroom := fs.Int("burst-headroom", 6000, "medium VMs the pool must still fit after seeding")
	image := fs.String("fake-image", "spike1b.local/fakeprovider:dev", "fake provider image")
	fakeEnv := fs.String("fake-env", "", "extra fake provider env, NAME=value;NAME=value")
	return func(ctx context.Context, e *env) error {
		l := &layout{Namespaces: *namespaces}
		// Average seeded VM: 2.6 vCPU / 8 GiB (workload.ClassFor). Size hosts so
		// the seed fills about half and the burst headroom fits on top.
		perHost := float64(*seed) / float64(*hosts)
		burstPerHost := float64(*burstHeadroom) / float64(*hosts)
		l.HostCPU = int32(max(64, 2*2.6*perHost+2*burstPerHost+8))
		l.HostMemMiB = int64(max(262144, 2*8192*perHost+4096*burstPerHost+16384))
		for p := 0; p < *providers; p++ {
			pl := providerLayout{Name: fmt.Sprintf("fake-p%d", p), HostPrefix: fmt.Sprintf("p%d-host-", p), Hosts: *hosts / *providers}
			pl.Seed = workload.Spec{VMPrefix: fmt.Sprintf("s%d-", p), Count: *seed / *providers, Namespaces: *namespaces,
				HostPrefix: pl.HostPrefix, Hosts: pl.Hosts}
			l.Providers = append(l.Providers, pl)
		}
		if err := ensureNamespace(ctx, e, infraNamespace); err != nil {
			return err
		}
		for n := 0; n < *namespaces; n++ {
			ns := workload.Namespace(n)
			if err := ensureNamespace(ctx, e, ns); err != nil {
				return err
			}
			image := &v1beta1.VMImage{
				ObjectMeta: metav1.ObjectMeta{Name: imageName, Namespace: ns},
				Spec: v1beta1.VMImageSpec{Source: v1beta1.ImageSource{
					Libvirt: &v1beta1.LibvirtImageSource{Path: "/var/lib/libvirt/images/spike1b-base.qcow2", Format: "qcow2"},
				}},
			}
			if err := createIfAbsent(ctx, e, image); err != nil {
				return err
			}
			for _, c := range workload.Classes {
				class := &v1beta1.VMClass{
					ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: ns},
					Spec:       v1beta1.VMClassSpec{CPU: c.CPU, Memory: *resource.NewQuantity(c.MemoryMiB*mib, resource.BinarySI)},
				}
				if err := createIfAbsent(ctx, e, class); err != nil {
					return err
				}
			}
		}
		data, err := json.MarshalIndent(l, "", "  ")
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: layoutConfigMap, Namespace: infraNamespace}, Data: map[string]string{layoutKey: string(data)}}
		if err := createIfAbsent(ctx, e, cm); err != nil {
			return err
		}
		// Placeholder SSH material: the Provider controller inlines a key per
		// Host into the inventory Secret; the fake agents never use it.
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credsSecret, Namespace: infraNamespace},
			StringData: map[string]string{
				"ssh-privatekey": "spike1b-placeholder-not-an-ssh-key\n",
				"known_hosts":    "# spike1b placeholder\n",
			},
		}
		if err := createIfAbsent(ctx, e, secret); err != nil {
			return err
		}
		extraEnv := parseEnvList(*fakeEnv)
		for _, pl := range l.Providers {
			if err := createProvider(ctx, e, pl, l, *image, extraEnv); err != nil {
				return err
			}
		}
		fmt.Printf("setup done: %d provider(s), %d hosts, %d seeded VMs, host %d vCPU / %d MiB\n",
			len(l.Providers), *hosts, *seed, l.HostCPU, l.HostMemMiB)
		return nil
	}
}

// parseEnvList reads NAME=value;NAME=value.
func parseEnvList(s string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, kv := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			out = append(out, corev1.EnvVar{Name: k, Value: v})
		}
	}
	return out
}

// createProvider creates one Provider, its HostPool and its Hosts.
func createProvider(ctx context.Context, e *env, pl providerLayout, l *layout, image string, extraEnv []corev1.EnvVar) error {
	envVars := []corev1.EnvVar{
		{Name: "FAKE_SEED", Value: pl.Seed.String()},
		{Name: "FAKE_HOST_CPU", Value: strconv.Itoa(int(l.HostCPU))},
		{Name: "FAKE_HOST_MEM_MIB", Value: strconv.FormatInt(l.HostMemMiB, 10)},
	}
	envVars = append(envVars, extraEnv...)
	provider := &v1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: pl.Name, Namespace: infraNamespace},
		Spec: v1beta1.ProviderSpec{
			Type:                      v1beta1.ProviderTypeLibvirt,
			Topology:                  v1beta1.ProviderTopologyCluster,
			Endpoint:                  "qemu+ssh://agent@placeholder/system",
			CredentialSecretRef:       v1beta1.ObjectRef{Name: credsSecret},
			ConsumerNamespaceSelector: &metav1.LabelSelector{},
			Runtime: &v1beta1.ProviderRuntimeSpec{
				Mode:            v1beta1.RuntimeModeRemote,
				Image:           image,
				ImagePullPolicy: corev1.PullNever,
				Service:         &v1beta1.ProviderServiceSpec{Port: 9443, TLS: &v1beta1.ProviderTLSSpec{Enabled: false}},
				NodeSelector:    map[string]string{"spike1b/role": "workload"},
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi")},
				},
				Env: envVars,
			},
		},
	}
	if err := createIfAbsent(ctx, e, provider); err != nil {
		return err
	}
	pool := &v1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: pl.Name + "-" + poolName, Namespace: infraNamespace},
		Spec: v1beta1.HostPoolSpec{
			ProviderRef: v1beta1.LocalObjectReference{Name: pl.Name},
			Strategy:    v1beta1.PoolStrategySpread,
			Overcommit:  &v1beta1.OvercommitRatios{CPU: "1.0", Memory: "1.0"},
		},
	}
	if err := createIfAbsent(ctx, e, pool); err != nil {
		return err
	}
	return parallel(ctx, pl.Hosts, 32, func(i int) error {
		name := workload.HostName(pl.HostPrefix, i)
		h := &v1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: infraNamespace},
			Spec: v1beta1.HostSpec{
				ProviderRef: v1beta1.LocalObjectReference{Name: pl.Name},
				PoolRef:     v1beta1.LocalObjectReference{Name: pool.Name},
				Endpoint:    fmt.Sprintf("grpc://%s.agents.spike1b:7443", name),
				Schedulable: true,
			},
		}
		return createIfAbsent(ctx, e, h)
	})
}

// ensureNamespace creates ns if needed.
func ensureNamespace(ctx context.Context, e *env, ns string) error {
	return createIfAbsent(ctx, e, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
}

// createIfAbsent creates obj, ignoring AlreadyExists.
func createIfAbsent(ctx context.Context, e *env, obj client.Object) error {
	if err := e.c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// parallel runs fn(0..n-1) on workers goroutines; the first error wins.
func parallel(ctx context.Context, n, workers int, fn func(i int) error) error {
	work := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := fn(i); err != nil {
					once.Do(func() { first = err })
				}
			}
		}()
	}
	start := time.Now()
	last := start
	for i := 0; i < n; i++ {
		select {
		case work <- i:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return ctx.Err()
		}
		if time.Since(last) > 10*time.Second {
			last = time.Now()
			fmt.Printf("  %d/%d (%.0f/s)\n", i, n, float64(i)/time.Since(start).Seconds())
		}
	}
	close(work)
	wg.Wait()
	return first
}
