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
package journal_test

import (
	"context"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

func TestOpen_RefusesNoSync(t *testing.T) {
	journal.SetTestBoltOptions(func(o *bolt.Options) { o.NoSync = true })
	defer journal.SetTestBoltOptions(nil)
	_, err := journal.Open(context.Background(), journal.Options{
		Dir: journaltest.FastTempDir(t), FS: journaltest.NewMemFS(), Buckets: []string{testBucket},
	})
	if !errors.Is(err, journal.ErrNoSync) {
		t.Fatalf("err = %v, want ErrNoSync", err)
	}
}
