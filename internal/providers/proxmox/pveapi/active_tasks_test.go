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

package pveapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestListActiveTasks_AsksForMoreThanPVEsDefaultLimit: PVE lists at most 50
// tasks unless asked for more, also for source=active, so the request names
// its own limit.
func TestListActiveTasks_AsksForMoreThanPVEsDefaultLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api2/json/nodes/pve/tasks" || q.Get("source") != "active" ||
			q.Get("limit") != strconv.Itoa(activeTasksLimit) {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"data":[{"upid":"UPID:pve:1:qmclone:9000:u@pve!t:","type":"qmclone","id":"9000","user":"u@pve!t"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient(&Config{Endpoint: srv.URL, TokenID: "u@pve!t", TokenSecret: "s"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tasks, err := c.ListActiveTasks(context.Background(), "pve")
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Type != "qmclone" || tasks[0].User != "u@pve!t" {
		t.Fatalf("tasks = %+v", tasks)
	}
}
