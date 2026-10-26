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

package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// causeKeepingError is a provider error that keeps a cause for the provider's
// own log out of its gRPC status, as a staged-disk delete's error does.
type causeKeepingError struct{ msg, cause string }

func (e *causeKeepingError) Error() string { return e.msg + ": " + e.cause }

func (e *causeKeepingError) GRPCStatus() *status.Status { return status.New(codes.Unavailable, e.msg) }

// TestLoggingUnaryInterceptor_LogsTheWireErrorOnly: the request log prints the
// code and message the caller receives, never an error's own text, which can
// carry a cause kept off the wire (R28: a staged object's location).
func TestLoggingUnaryInterceptor_LogsTheWireErrorOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	interceptor := loggingUnaryInterceptor(&LoggingConfig{Enabled: true, Logger: logger})
	info := &grpc.UnaryServerInfo{FullMethod: "/provider.v1.Provider/DeleteStagedDisk"}

	leak := "qemu-img: Could not open 'nfs://nfs.example.com/exports/x.qcow2?uid=1000&gid=1000'"
	_, err := interceptor(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, &causeKeepingError{msg: "DeleteStagedDisk: the staged object could not be emptied", cause: leak}
	})
	if err == nil {
		t.Fatal("the handler's error is returned")
	}
	out := buf.String()
	for _, l := range []string{"nfs.example.com", "/exports", "uid=", "gid="} {
		if strings.Contains(out, l) {
			t.Errorf("the request log leaks %q: %s", l, out)
		}
	}
	if !strings.Contains(out, "the staged object could not be emptied") || !strings.Contains(out, "code=Unavailable") {
		t.Errorf("the request log names the wire code and message: %s", out)
	}

	buf.Reset()
	if _, err := interceptor(context.Background(), nil, info, func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "code=OK") {
		t.Errorf("a successful call logs code OK: %s", buf.String())
	}
}
