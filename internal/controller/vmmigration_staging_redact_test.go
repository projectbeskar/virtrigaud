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
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin R28 on the manager side (v0.4.1-rc2 lab finding 2b): the
// staging URL of an ExportDisk / ImportDisk is logged as its scheme only, and
// a provider answer that names the staging storage — whichever provider sent
// it — reaches neither the manager log nor the VMMigration's status,
// conditions or events.

const (
	r28MgrNFSServer = "nfs.example.com"
	r28MgrNFSExport = "/exports/tenant-a"
	r28MgrNFSPath   = "migrations/team-a"
	r28MgrBucket    = "tenant-bucket"
	r28MgrEndpoint  = "https://s3.example.com:9000"
)

// r28MgrLeaks are the texts that locate the fixtures' staging storage.
var r28MgrLeaks = []string{r28MgrNFSServer, r28MgrNFSExport, r28MgrNFSPath, "uid=", "gid=", r28MgrBucket,
	"s3.example.com"}

// leakyExportProvider answers ExportDisk and ImportDisk with err, a provider
// answer that quotes the staging location.
type leakyExportProvider struct {
	stubProvider
	err error
}

func (p *leakyExportProvider) ExportDisk(context.Context, contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	return contracts.ExportDiskResponse{}, p.err
}

func (p *leakyExportProvider) ImportDisk(context.Context, contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	return contracts.ImportDiskResponse{}, p.err
}

// nfsStaging is a staging configuration on NFS with a uid/gid.
func nfsStaging() *infrav1beta1.MigrationStorage {
	uid, gid := int64(1000), int64(1000)
	return &infrav1beta1.MigrationStorage{Type: "nfs", NFS: &infrav1beta1.NFSStorageConfig{
		Server: r28MgrNFSServer, Export: r28MgrNFSExport, Path: r28MgrNFSPath, UID: &uid, GID: &gid}}
}

// nfsStagedExport is raceMigrationFixture staging on NFS, with its source VM,
// source Provider and target Provider.
func nfsStagedExport() []client.Object {
	vm, p, m := raceMigrationFixture()
	m.Spec.Storage = nfsStaging()
	m.Status.StoragePVCName = ""
	target, _ := importRaceFixture()
	return []client.Object{vm, p, target, m}
}

// capturedLog collects the lines a funcr logger writes.
type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturedLog) ctx() context.Context {
	return log.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 1}))
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

// TestMigrationExport_StagingLocationNeverLogged: the "Starting disk export"
// line carries the destination's scheme only; a definite provider failure
// that quotes the staging location fails the migration with a status,
// conditions, event and log lines that do not.
func TestMigrationExport_StagingLocationNeverLogged(t *testing.T) {
	leak := "qemu-img: Could not open 'nfs://" + r28MgrNFSServer + r28MgrNFSExport + "/" + r28MgrNFSPath +
		"/vmmigrations_default_race-migration_export.qcow2?gid=1000&uid=1000': MNT3ERR_ACCES (" +
		r28MgrNFSServer + ":" + r28MgrNFSExport + ")"
	for name, answer := range map[string]error{
		"a definite failure fails the migration":  contracts.NewInvalidSpecError(leak, nil),
		"an unknown outcome keeps the marker":     errors.New(leak),
		"a not-performed answer fails it as well": contracts.NewHostUnavailableError(leak, nil),
	} {
		t.Run(name, func(t *testing.T) {
			objs := nfsStagedExport()
			m := objs[len(objs)-1].(*infrav1beta1.VMMigration)
			c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(objs...).
				WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
			r := migrationReconcilerOn(c, &leakyExportProvider{err: answer})
			rec := record.NewFakeRecorder(50)
			r.Recorder = rec
			logs := &capturedLog{}

			_, err := r.handleExportingPhase(logs.ctx(), storedMigration(t, c, m))
			require.NoError(t, err)

			out := logs.String()
			assert.Contains(t, out, "Starting disk export")
			assert.Contains(t, out, `"destination"="nfs://<redacted>"`)
			got := storedMigration(t, c, m)
			texts := append([]string{out, got.Status.Message}, drainEvents(rec)...)
			for _, cond := range got.Status.Conditions {
				texts = append(texts, cond.Message)
			}
			for _, text := range texts {
				for _, l := range r28MgrLeaks {
					assert.NotContains(t, text, l)
				}
			}
			if contracts.IsInvalidSpec(answer) {
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				assert.Contains(t, got.Status.Message, "MNT3ERR_ACCES", "the provider's answer is kept, redacted")
			}
		})
	}
}

// TestMigrationImport_StagingLocationNeverLogged: the "Starting disk import"
// line carries the source's scheme only.
func TestMigrationImport_StagingLocationNeverLogged(t *testing.T) {
	p, m := importRaceFixture()
	m.Spec.Storage = nfsStaging()
	m.Status.StoragePVCName = ""
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).WithObjects(p, m).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	r := migrationReconcilerOn(c, &leakyExportProvider{err: contracts.NewInvalidSpecError(
		"open nfs://"+r28MgrNFSServer+r28MgrNFSExport+"/x.qcow2?uid=1000&gid=1000 failed", nil)})
	logs := &capturedLog{}
	_, err := r.handleImportingPhase(logs.ctx(), storedMigration(t, c, m))
	require.NoError(t, err)
	out := logs.String()
	assert.Contains(t, out, "Starting disk import")
	assert.Contains(t, out, `"source"="nfs://<redacted>"`)
	got := storedMigration(t, c, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	for _, text := range []string{out, got.Status.Message} {
		for _, l := range r28MgrLeaks {
			assert.NotContains(t, text, l)
		}
	}
}

// TestRedactMigrationStagingText: every staging location of the spec — the S3
// bucket and endpoint, the NFS server, export and path — and every URL are
// replaced; the rest of the text is kept.
func TestRedactMigrationStagingText(t *testing.T) {
	m := &infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{Storage: &infrav1beta1.MigrationStorage{
		Type: "s3", S3: &infrav1beta1.S3StorageConfig{Bucket: r28MgrBucket, Endpoint: r28MgrEndpoint}}}}
	got := redactMigrationStagingText(m, `Failed to start disk export: S3 upload failed: Put "`+r28MgrEndpoint+"/"+
		r28MgrBucket+`/k": dial tcp s3.example.com:9000: connection refused; bucket `+r28MgrBucket)
	for _, l := range r28MgrLeaks {
		assert.NotContains(t, got, l)
	}
	assert.Contains(t, got, "Failed to start disk export: S3 upload failed")
	assert.Contains(t, got, "connection refused")

	assert.Equal(t, "plain", redactMigrationStagingText(&infrav1beta1.VMMigration{}, "plain"))
	assert.NoError(t, redactMigrationStagingError(m, nil))
	wrapped := redactMigrationStagingError(m, contracts.NewInvalidSpecError("bucket "+r28MgrBucket, nil))
	assert.NotContains(t, wrapped.Error(), r28MgrBucket)
	assert.True(t, contracts.IsInvalidSpec(wrapped), "the chain is kept")
}
