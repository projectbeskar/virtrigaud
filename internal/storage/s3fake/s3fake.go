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

// Package s3fake is a minimal in-process S3 server for tests: path-style
// HEAD, GET, PUT and DELETE of objects in one bucket, and multipart uploads
// (initiate, upload part, complete, abort), with no signature checks. It
// records every request and can be told to fail them, so a test can prove
// exactly which keys a client touched.
package s3fake

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// fakeUploadID is the id every multipart upload of the fake gets (one upload
// at a time per key is enough for the tests that use it).
const fakeUploadID = "fake-upload-id"

// Request is one request the server received.
type Request struct {
	// Method is the HTTP method.
	Method string
	// Bucket is the first path segment.
	Bucket string
	// Key is the rest of the path (the object key).
	Key string
}

// Server is a fake S3 endpoint. Create it with New and Close it when done.
type Server struct {
	srv    *httptest.Server
	bucket string

	mu       sync.Mutex
	objects  map[string][]byte
	reported map[string]int64
	requests []Request
	failCode int
	// parts holds the parts of the multipart uploads in flight, by key.
	parts map[string]map[int][]byte
}

// ReportSize makes HEAD report size for key while GET serves the stored
// object in full (without a Content-Length): a backend that lies about an
// object's size.
func (s *Server) ReportSize(key string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reported == nil {
		s.reported = map[string]int64{}
	}
	s.reported[key] = size
}

// New starts a fake S3 server holding the one bucket named bucket.
func New(bucket string) *Server {
	s := &Server{bucket: bucket, objects: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Endpoint is the server's base URL ("http://127.0.0.1:<port>"), usable as an
// S3 endpoint with path-style addressing.
func (s *Server) Endpoint() string { return s.srv.URL }

// Put stores an object.
func (s *Server) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
}

// Has reports whether key is stored.
func (s *Server) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// Keys returns every stored key.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	return out
}

// Requests returns every request received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// FailWith makes every following request answer HTTP code (0 stops failing).
func (s *Server) FailWith(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCode = code
}

// serve handles one request.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Bucket: bucket, Key: key})
	failCode := s.failCode
	s.mu.Unlock()

	if failCode != 0 {
		writeError(w, failCode, "InternalError")
		return
	}
	if bucket != s.bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveMultipart(w, r, key) {
		return
	}
	data, found := s.objects[key]
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		if !found {
			writeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		size, lie := s.reported[key]
		switch {
		case !lie:
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "BadRequest")
			return
		}
		s.objects[key] = body
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// writeError answers an S3 XML error.
func writeError(w http.ResponseWriter, code int, s3Code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+s3Code+`</Code><Message>`+s3Code+`</Message></Error>`)
}

// serveMultipart answers the multipart upload requests (s.mu is held) and
// reports whether r was one.
func (s *Server) serveMultipart(w http.ResponseWriter, r *http.Request, key string) bool {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		if s.parts == nil {
			s.parts = map[string]map[int][]byte{}
		}
		s.parts[key] = map[int][]byte{}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>`+html.EscapeString(s.bucket)+
			`</Bucket><Key>`+html.EscapeString(key)+`</Key><UploadId>`+fakeUploadID+`</UploadId></InitiateMultipartUploadResult>`)
		return true
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		n, err := strconv.Atoi(q.Get("partNumber"))
		body, rerr := io.ReadAll(r.Body)
		if err != nil || rerr != nil || s.parts[key] == nil {
			writeError(w, http.StatusBadRequest, "BadRequest")
			return true
		}
		s.parts[key][n] = body
		w.Header().Set("ETag", `"part-`+strconv.Itoa(n)+`"`)
		w.WriteHeader(http.StatusOK)
		return true
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		_, _ = io.Copy(io.Discard, r.Body)
		parts := s.parts[key]
		if parts == nil {
			writeError(w, http.StatusNotFound, "NoSuchUpload")
			return true
		}
		var data []byte
		for n := 1; n <= len(parts); n++ {
			data = append(data, parts[n]...)
		}
		s.objects[key] = data
		delete(s.parts, key)
		w.Header().Set("Content-Type", "application/xml")
		bucket, escaped := html.EscapeString(s.bucket), html.EscapeString(key)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Location>/`+bucket+"/"+escaped+
			`</Location><Bucket>`+bucket+`</Bucket><Key>`+escaped+`</Key><ETag>"fake"</ETag></CompleteMultipartUploadResult>`)
		return true
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(s.parts, key)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}
