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

package storage_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
)

func TestRedactLocations(t *testing.T) {
	text := `qemu-img: Could not open 'nfs://nfs.example.com/exports/mig/vmmigrations_ns_m_export.qcow2?gid=1000&uid=1000': ` +
		`mount failed for nfs.example.com:/exports/mig; Put "https://s3.example.com/tenant-bucket/k?x=1": EOF`
	got := storage.RedactLocations(text, "nfs.example.com", "/exports/mig", "tenant-bucket", "/", "ab")
	for _, leak := range []string{"nfs.example.com", "/exports/mig", "uid=", "gid=", "tenant-bucket", "s3.example.com"} {
		assert.NotContains(t, got, leak)
	}
	assert.Contains(t, got, "<redacted nfs URL>")
	assert.Contains(t, got, "<redacted https URL>")
	assert.Contains(t, got, storage.StagedLocationMarker)
	assert.Contains(t, got, "qemu-img: Could not open", "the rest of the text is kept")
	assert.Equal(t, "a/b ab", storage.RedactLocations("a/b ab", "/", "ab"), "texts too short to redact safely are ignored")

	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	err := storage.RedactError(errors.Join(errors.New("tenant-bucket"), cause), "tenant-bucket")
	assert.NotContains(t, err.Error(), "tenant-bucket")
	var oe *net.OpError
	assert.ErrorAs(t, err, &oe, "the chain is kept for classification")
	assert.NoError(t, storage.RedactError(nil))
}

func TestS3EndpointLocations(t *testing.T) {
	assert.Nil(t, storage.S3EndpointLocations(" "))
	assert.Equal(t, []string{"http://s3.example.com:9000", "s3.example.com:9000", "s3.example.com"},
		storage.S3EndpointLocations("http://s3.example.com:9000"))
	assert.Equal(t, []string{"s3.example.com", "s3.example.com"}, storage.S3EndpointLocations("s3.example.com"))
}

// TestS3Errors_NameNoLocation: an S3 failure — a refusal by the service, a
// service that cannot be reached — names neither the bucket, the key nor the
// endpoint in its error text (R28), which every provider logs and some put on
// the wire.
func TestS3Errors_NameNoLocation(t *testing.T) {
	const bucket, key = "tenant-bucket", "vmmigrations/team-a/web/export.vmdk"
	newClient := func(t *testing.T, endpoint string) *storage.S3Storage {
		t.Helper()
		s, err := storage.NewS3Storage(storage.StorageConfig{Type: "s3", S3: &storage.S3Config{
			Bucket: bucket, Endpoint: endpoint, AccessKeyID: "AK", SecretAccessKey: "SK", UsePathStyle: true}})
		require.NoError(t, err)
		return s
	}
	assertClean := func(t *testing.T, err error, endpoint string) {
		t.Helper()
		require.Error(t, err)
		host := strings.TrimPrefix(endpoint, "http://")
		for _, leak := range []string{bucket, key, "export.vmdk", host, "SK"} {
			assert.NotContains(t, err.Error(), leak)
		}
	}

	t.Run("the service refuses the upload", func(t *testing.T) {
		srv := s3fake.New(bucket)
		t.Cleanup(srv.Close)
		srv.FailWith(403)
		s := newClient(t, srv.Endpoint())
		_, err := s.UploadStream(context.Background(), storage.StreamUploadRequest{
			DestinationURL: "s3://" + bucket + "/" + key, Reader: strings.NewReader("disk"), ContentLength: -1})
		assertClean(t, err, srv.Endpoint())
		assert.Contains(t, err.Error(), "S3 upload failed")
	})
	t.Run("the service cannot be reached", func(t *testing.T) {
		srv := s3fake.New(bucket)
		endpoint := srv.Endpoint()
		srv.Close()
		s := newClient(t, endpoint)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := s.GetMetadata(ctx, "s3://"+bucket+"/"+key)
		assertClean(t, err, endpoint)
		_, err = s.RemoveObjectIfExists(ctx, key)
		assertClean(t, err, endpoint)
		err = s.Delete(ctx, "s3://"+bucket+"/"+key)
		assertClean(t, err, endpoint)
	})
	t.Run("an endpoint that is not a URL is not echoed", func(t *testing.T) {
		_, err := storage.NewS3Storage(storage.StorageConfig{Type: "s3", S3: &storage.S3Config{
			Bucket: bucket, Endpoint: "http://s3.example.com:bad port", AccessKeyID: "AK", SecretAccessKey: "SK"}})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "s3.example.com")
	})
}
