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

package migration

import (
	"errors"
	"path"
	"strings"

	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// StagingLocations returns the texts that locate the staged object of an
// ExportDisk or ImportDisk request, for storage.RedactLocations (R28): the
// staging URL rawURL and the texts in it (an nfs:// URL's server, path,
// directories and file name — NFSURLLocations and the path's parent
// directories, so that the export is found in a mount error that names
// "<server>:<export>"; an s3:// URL's object key), and the non-secret storage
// options of optionsJSON (the bucket, prefix and endpoint, the nfs server,
// export and path). An unparsable optionsJSON contributes nothing.
func StagingLocations(rawURL, optionsJSON string) []string {
	rawURL = strings.TrimSpace(rawURL)
	opts, _ := ParseStorageOptions(optionsJSON)
	var out []string
	switch {
	case strings.HasPrefix(rawURL, "nfs://"):
		out = NFSURLLocations(rawURL)
		if len(out) > 1 {
			for dir := path.Dir(out[1]); dir != "/" && dir != "."; dir = path.Dir(dir) {
				out = append(out, dir)
			}
		}
	case strings.HasPrefix(rawURL, "s3://"):
		rest := strings.TrimPrefix(rawURL, "s3://")
		key := rest
		if bucket, k, ok := strings.Cut(rest, "/"); ok && bucket == strings.TrimSpace(opts.Bucket) {
			key = k
		}
		out = append(out, rawURL, key)
	}
	out = append(out, StagedDiskLocations(StagedDisk{Options: opts})...)
	return append(out, strings.Trim(opts.Prefix, "/"), opts.Server, strings.TrimRight(opts.Export, "/"),
		strings.Trim(opts.Path, "/"))
}

// RedactStagingError returns err as a provider RPC answers it, with every URL
// and every one of locations (StagingLocations) redacted from its text
// (storage.RedactLocations), for a provider whose errors can quote host
// command output or a storage client's error. gRPC puts an error's whole text
// on the wire and in the SDK's request log, so it is redacted at the RPC
// boundary:
//
//   - an SDK *ProviderError keeps its code, retryability and details; its
//     message and cause are redacted (its status message is both);
//   - an error that carries another gRPC status keeps its code and details;
//     its message is redacted;
//   - any other error is redacted with its chain kept (storage.RedactError).
//
// nil stays nil.
func RedactStagingError(err error, locations ...string) error {
	if err == nil {
		return nil
	}
	if pe, ok := err.(*sdkerrors.ProviderError); ok { //nolint:errorlint // the top-level error is what gRPC renders
		c := *pe
		c.Message = storage.RedactLocations(pe.Message, locations...)
		if pe.Cause != nil {
			c.Cause = storage.RedactError(pe.Cause, locations...)
		}
		return &c
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		p := status.Convert(err).Proto()
		p.Message = storage.RedactLocations(p.GetMessage(), locations...)
		return status.FromProto(p).Err()
	}
	return storage.RedactError(err, locations...)
}
