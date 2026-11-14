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

// Package agenttarget presents real per-host agent processes, behind a thin
// in-test router, as a clustered conformance Target (clustered.Target). It is
// test-only and exists only in development builds (build tag
// virtrigaud_dev): each host is a separate operating-system process that
// runs the agent's start-up sequence, its operation store, its gRPC server
// with mutual TLS and the development fake executor, so that the identity.*
// cases, and the crash-point tests that kill an agent with SIGKILL, run
// against the agent itself rather than an in-memory stand-in.
//
// The router stands in for a clustered Provider: it routes a signed call by
// a decoded copy of the request inside the envelope, forwards the outer
// message unchanged over the host's mutual TLS connection, encodes the host
// into every task reference it returns, and routes a TaskStatus or Describe
// that carries answer_nonce by its target_host_id. It verifies nothing
// itself: every decision is the agent's.
//
// The test binary that uses a Target must call RunChildIfRequested first in
// its TestMain: the agent processes are that binary, re-executed.
package agenttarget
