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

package contracts

import "context"

// ImagePrepareRequest contains the information needed to prepare/import a VM
// image into a provider so it can back subsequent VM creation. It is the
// manager-side, transport-agnostic mirror of the provider.v1 ImagePrepareRequest
// message (issue #154).
type ImagePrepareRequest struct {
	// ImageJSON is the JSON-encoded VMImage spec describing the image source
	// (e.g. source.vsphere.ovaURL, source.libvirt.path/url, source.proxmox.*).
	ImageJSON string
	// TargetName is the desired name of the prepared template/image on the
	// provider.
	TargetName string
	// StorageHint names the target storage location (vSphere datastore, libvirt
	// pool, Proxmox storage), or empty to let the provider choose.
	StorageHint string
}

// ImagePrepareResponse contains the result of an image-prepare operation.
type ImagePrepareResponse struct {
	// TaskRef references an async operation when the prepare is not synchronous;
	// an empty TaskRef means the operation completed synchronously.
	TaskRef string
}

// ImagePreparer is an optional capability of a Provider: it prepares/imports a
// VM image into the provider (download and/or convert into a template or storage
// pool) so it can back subsequent VM creation. The manager gRPC client implements
// it; callers type-assert a Provider to ImagePreparer to invoke a prepare without
// widening the core Provider interface, so in-process provider implementations and
// test fakes that do not support it are unaffected (issue #154). This mirrors the
// Cloner pattern.
type ImagePreparer interface {
	// PrepareImage prepares the image described by req into a template/image named
	// req.TargetName. Returns a TaskRef the caller can poll via IsTaskComplete when
	// the operation is asynchronous; an empty TaskRef means it completed
	// synchronously. The operation is idempotent: preparing an image that already
	// exists is a no-op success.
	PrepareImage(ctx context.Context, req ImagePrepareRequest) (ImagePrepareResponse, error)
}
