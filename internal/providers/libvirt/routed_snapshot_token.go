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

package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Idempotency of a CLUSTERED SnapshotCreate (ADR-0007 Addendum A, slice 3).
//
// A routed SnapshotCreate whose budget runs out is answered "outcome unknown,
// retry": libvirtd may still complete the snapshot. The retry must find THAT
// snapshot, not make a second one — and must never take a snapshot of the same
// name that something else left on the VM (an earlier VMSnapshot of the same
// name whose finalizer was dropped, a force-delete) for this request: a new
// VMSnapshot would then report Ready on an older snapshot, possibly of another
// memory mode, and a later revert would restore the wrong state.
//
// So the create is idempotent per REQUEST, not per name: the manager sends the
// request object's uid as request_token, the snapshot is created with the
// token recorded in its description (snapshotRequestTokenMarker), and an
// existing snapshot of the requested name is adopted only when it records the
// same token. Any other — no token, another token, or no token in the request
// (an older manager) — is ALREADY_EXISTS.

const (
	// snapshotRequestTokenPrefix and snapshotRequestTokenSuffix enclose the
	// request token recorded at the end of a routed snapshot's description:
	// "<description> [virtrigaud-request-token=<token>]".
	snapshotRequestTokenPrefix = "[virtrigaud-request-token="
	snapshotRequestTokenSuffix = "]"
)

// snapshotRequestTokenRE is the shape of a request token: a Kubernetes uid, or
// any short identifier of letters, digits, '.', '_' and '-'. It never contains
// the marker's brackets or a line break.
var snapshotRequestTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// checkSnapshotRequestToken refuses a request token that is not an identifier
// (InvalidSpec). An empty token is allowed: it names no request.
func checkSnapshotRequestToken(token string) error {
	if token == "" || snapshotRequestTokenRE.MatchString(token) {
		return nil
	}
	return contracts.NewInvalidSpecError("snapshot request token is not an identifier (letters, digits, '.', '_' or '-'; at most 128)", nil)
}

// defaultSnapshotDescription is the description of a snapshot created without
// one.
func defaultSnapshotDescription() string {
	return fmt.Sprintf("Snapshot created by VirtRigaud at %s", time.Now().Format(time.RFC3339))
}

// withSnapshotRequestToken returns req with its request token recorded at the
// end of its description (the default description when it has none). A
// request without a token is returned unchanged.
func withSnapshotRequestToken(req *providerv1.SnapshotCreateRequest) *providerv1.SnapshotCreateRequest {
	token := req.GetRequestToken()
	if token == "" {
		return req
	}
	out, ok := proto.Clone(req).(*providerv1.SnapshotCreateRequest)
	if !ok {
		return req
	}
	description := out.GetDescription()
	if description == "" {
		description = defaultSnapshotDescription()
	}
	out.Description = description + " " + snapshotRequestTokenPrefix + token + snapshotRequestTokenSuffix
	return out
}

// snapshotRequestToken returns the request token recorded in a snapshot's
// description ("" when none).
func snapshotRequestToken(description string) string {
	i := strings.LastIndex(description, snapshotRequestTokenPrefix)
	if i < 0 {
		return ""
	}
	rest := description[i+len(snapshotRequestTokenPrefix):]
	j := strings.Index(rest, snapshotRequestTokenSuffix)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// snapshotDescriptionDoc is the part of `virsh snapshot-dumpxml` read here.
type snapshotDescriptionDoc struct {
	XMLName     xml.Name `xml:"domainsnapshot"`
	Description string   `xml:"description"`
}

// routedSnapshotToken reads the request token recorded with snapshot name on
// domain (`virsh snapshot-dumpxml`).
func routedSnapshotToken(ctx context.Context, c libvirtConn, domain, name string) (string, error) {
	res, err := c.Virsh(ctx, "snapshot-dumpxml", domain, name)
	if err != nil {
		return "", fmt.Errorf("read snapshot %s: %w", name, err)
	}
	var doc snapshotDescriptionDoc
	if err := xml.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		return "", fmt.Errorf("parse snapshot %s: %w", name, err)
	}
	return snapshotRequestToken(doc.Description), nil
}

// existingRoutedSnapshot makes a CLUSTERED SnapshotCreate idempotent per
// request. When req names its snapshot (the manager always does) and a
// snapshot of that sanitized name already exists on domain — the
// owner-checked domain, so it is this VM's — it is this request's earlier
// attempt's only when it records req's request token: that snapshot is the
// answer. Any other is a Conflict (ALREADY_EXISTS on the wire): it is never
// adopted and never replaced. It returns nil when there is no such snapshot
// (or no name to look for), and an error when a check failed (never assuming
// either answer).
func existingRoutedSnapshot(ctx context.Context, c libvirtConn, domain string, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	if req.GetNameHint() == "" {
		return nil, nil
	}
	name := sanitizeSnapshotName(req.GetNameHint())
	exists, err := c.snapshotExists(ctx, domain, name)
	if err != nil {
		return nil, fmt.Errorf("check for an existing snapshot %s: %w", name, err)
	}
	if !exists {
		return nil, nil
	}
	token, err := routedSnapshotToken(ctx, c, domain, name)
	if err != nil {
		return nil, err
	}
	if want := req.GetRequestToken(); want != "" && token == want {
		log.Printf("INFO Snapshot %s on domain %s was made for this request; treating the create as done", name, domain)
		return &providerv1.SnapshotCreateResponse{SnapshotId: name}, nil
	}
	log.Printf("WARN Refusing to adopt snapshot %s on domain %s: it was not made for this request (recorded token %q)", name, domain, token)
	return nil, contracts.NewConflictError(fmt.Sprintf(
		"a snapshot named %q already exists on this VM and was not made for this request (for example one an earlier "+
			"snapshot of the same name left behind); delete it on the VM or use another snapshot name", name), nil)
}
