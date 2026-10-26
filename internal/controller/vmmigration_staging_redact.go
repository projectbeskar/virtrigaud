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
	"strings"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/storage"
)

// A VMMigration's staging storage — its S3 bucket and endpoint, its NFS
// server, export and path — and the staged object's URL (which also carries
// an nfs object's uid/gid) name the tenant's storage, and belong in no
// manager log line, status message, condition or event (R28). The providers
// redact them from what they answer and log; the manager logs the staging URL
// it sends as its scheme only (logging.RedactLocation), and redacts every
// provider answer it writes into the VMMigration's failure (the status
// message, the Failed and Ready conditions, the Warning event) or logs while
// it exports or imports the disk, whatever the provider: a provider's answer
// is not trusted to be clean.

// migrationStagingLocations returns the texts that locate m's staging
// storage, for storage.RedactLocations: the S3 bucket and endpoint (and the
// endpoint's host), the NFS server, export and path. Texts shorter than
// storage.MinRedactedLocationLen are not redacted.
func migrationStagingLocations(m *infrav1beta1.VMMigration) []string {
	if m == nil || m.Spec.Storage == nil {
		return nil
	}
	var out []string
	if s3 := m.Spec.Storage.S3; s3 != nil {
		out = append(out, s3.Bucket)
		out = append(out, storage.S3EndpointLocations(s3.Endpoint)...)
	}
	if nfs := m.Spec.Storage.NFS; nfs != nil {
		out = append(out, nfs.Server, strings.TrimRight(nfs.Export, "/"), strings.Trim(nfs.Path, "/"))
	}
	return out
}

// redactMigrationStagingText returns text with every URL in it reduced to its
// scheme and every one of m's staging locations (migrationStagingLocations)
// replaced (storage.RedactLocations).
func redactMigrationStagingText(m *infrav1beta1.VMMigration, text string) string {
	return storage.RedactLocations(text, migrationStagingLocations(m)...)
}

// redactMigrationStagingError is redactMigrationStagingText on err's text,
// for a log line; the returned error unwraps to err (nil stays nil).
func redactMigrationStagingError(m *infrav1beta1.VMMigration, err error) error {
	return storage.RedactError(err, migrationStagingLocations(m)...)
}
