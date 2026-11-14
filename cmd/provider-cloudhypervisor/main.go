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

// Command provider-cloudhypervisor is the clustered Provider that routes
// signed operations to per-host agents (vrtg-agent). It is for development
// builds only: a build without the virtrigaud_dev build tag links none of the
// Provider and refuses to run, and no release artefact builds or ships it.
//
// Flags (development builds):
//
//	--port                 gRPC port the manager connects to (default 9443)
//	--health-port          HTTP health port (default 8080)
//	--hosts-file           the mounted host inventory
//	                       (default /etc/virtrigaud/hosts/hosts.json)
//	--agent-client-cert    PEM certificate the Provider presents to agents
//	--agent-client-key     PEM key of that certificate
//	--provider-uid         UID of the Provider resource; agents enrolled with
//	                       another Provider are refused. Optional for now (a
//	                       warning is logged when unset); a deployment the
//	                       manager renders must set it
//
// The manager-facing TLS follows the other Providers (the mounted provider
// TLS material and its environment contract).
package main

import "os"

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// Exit statuses of realMain.
const (
	exitOK    = 0
	exitError = 1
)
