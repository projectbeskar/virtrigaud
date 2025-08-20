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

package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/projectbeskar/virtrigaud/api/v1alpha1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// ProviderFactory creates a new provider instance
type ProviderFactory func(ctx context.Context, provider *v1alpha1.Provider) (contracts.Provider, error)

// Registry manages provider factories and instances
type Registry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
	instances map[string]contracts.Provider
}

// NewRegistry creates a new provider registry
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]ProviderFactory),
		instances: make(map[string]contracts.Provider),
	}
}

// Register registers a provider factory for a given type
func (r *Registry) Register(providerType string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[providerType] = factory
}

// Get returns a provider instance for the given Provider resource
func (r *Registry) Get(ctx context.Context, provider *v1alpha1.Provider) (contracts.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Create a cache key from provider spec
	cacheKey := fmt.Sprintf("%s:%s:%s", provider.Spec.Type, provider.Namespace, provider.Name)

	// Check if we have a cached instance
	if instance, ok := r.instances[cacheKey]; ok {
		return instance, nil
	}

	// Look up the factory
	factory, ok := r.factories[provider.Spec.Type]
	if !ok {
		return nil, fmt.Errorf("no factory registered for provider type: %s", provider.Spec.Type)
	}

	// Create new instance
	instance, err := factory(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider instance: %w", err)
	}

	// Cache the instance
	r.instances[cacheKey] = instance

	return instance, nil
}

// Invalidate removes a provider instance from the cache
func (r *Registry) Invalidate(provider *v1alpha1.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cacheKey := fmt.Sprintf("%s:%s:%s", provider.Spec.Type, provider.Namespace, provider.Name)
	delete(r.instances, cacheKey)
}

// ListSupportedTypes returns the list of supported provider types
func (r *Registry) ListSupportedTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.factories))
	for providerType := range r.factories {
		types = append(types, providerType)
	}
	return types
}

// IsSupported returns true if the provider type is supported
func (r *Registry) IsSupported(providerType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.factories[providerType]
	return ok
}
