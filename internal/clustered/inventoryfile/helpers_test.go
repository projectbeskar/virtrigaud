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

package inventoryfile

import (
	"io"
	"log/slog"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
)

// host is an SSH host of an inventory, with a valid endpoint built from a
// short token.
func host(id, endpoint string, key, known []byte) hostsecret.Host {
	return hostsecret.Host{
		ID:          id,
		Endpoint:    "qemu+ssh://virt@" + endpoint + "/system",
		Credentials: hostsecret.Credentials{SSHPrivateKey: key, KnownHosts: known},
	}
}

// inv is an inventory of hosts at the current schema version.
func inv(hosts ...hostsecret.Host) hostsecret.Inventory {
	return hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: hosts}
}

// quietLogger discards output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
