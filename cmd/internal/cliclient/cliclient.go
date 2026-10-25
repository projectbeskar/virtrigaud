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

// Package cliclient builds the Kubernetes clients shared by the VirtRigaud
// command-line tools (vrtg, vcts, virtrigaud-loadgen). It exists so every tool
// uses the same scheme, which must know the VirtRigaud API types, and the same
// kubeconfig resolution, which must honour the tools' --kubeconfig flag.
package cliclient

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// NewScheme returns a scheme that knows the built-in Kubernetes types and the
// VirtRigaud infra.virtrigaud.io/v1beta1 types. It mirrors the scheme the
// manager builds in cmd/manager.
//
// It returns an error when a type group cannot be registered.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add client-go types to scheme: %w", err)
	}
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add infra.virtrigaud.io/v1beta1 types to scheme: %w", err)
	}
	return scheme, nil
}

// RESTConfig resolves the cluster connection settings. A non-empty kubeconfig
// is the path of the kubeconfig file to use (the tools' --kubeconfig flag);
// otherwise the standard resolution applies (KUBECONFIG, in-cluster
// configuration, then ~/.kube/config).
//
// It returns an error when no configuration can be loaded.
func RESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		cfg, err := config.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
		return cfg, nil
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
	}
	return cfg, nil
}

// NewClient returns a controller-runtime client for cfg whose scheme knows the
// VirtRigaud API types.
//
// It returns an error when the scheme or the client cannot be built.
func NewClient(cfg *rest.Config) (client.Client, error) {
	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return c, nil
}
