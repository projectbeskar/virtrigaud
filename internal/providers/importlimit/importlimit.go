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

// Package importlimit enforces ImportDiskRequest.max_virtual_size_bytes, the
// target Provider's disk-size limit, on the provider side of an ImportDisk.
//
// The manager checks the size it recorded when the disk was exported, but the
// object in the staging location (a tenant's NFS export or staging PVC) can be
// replaced between the export and the import. So every provider reads the
// virtual size of the disk it is ABOUT TO LAND — in a location the tenant
// cannot change: a private copy, or the converted output — and calls Check
// once, right before landing it. A refusal is codes.FailedPrecondition with a
// google.rpc.ErrorInfo reason (contracts.DiskSizeExceedsLimitReason or
// contracts.DiskSizeUnverifiableReason) and a constant message; nothing is
// landed.
package importlimit

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// ExceedsMessage is the message of an import refused because the disk is
// above the limit.
const ExceedsMessage = "the disk to import has a virtual size above the target Provider's disk-size limit: nothing was imported"

// UnverifiableMessage is the message of an import refused because a limit
// applies and the disk's virtual size could not be read.
const UnverifiableMessage = "the virtual size of the disk to import could not be read, and the target Provider has a disk-size limit: nothing was imported"

// Refusal is an ImportDisk refused by the disk-size limit. It renders as
// codes.FailedPrecondition with its ErrorInfo reason (GRPCStatus), also
// through %w wrapping.
type Refusal struct {
	reason  string
	message string
}

// Error returns the refusal's constant message.
func (r *Refusal) Error() string { return r.message }

// Reason returns the refusal's ErrorInfo reason.
func (r *Refusal) Reason() string { return r.reason }

// GRPCStatus renders the refusal as codes.FailedPrecondition carrying a
// google.rpc.ErrorInfo{Reason: reason, Domain: contracts.ErrorInfoDomain}.
func (r *Refusal) GRPCStatus() *status.Status {
	st := status.New(codes.FailedPrecondition, r.message)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: r.reason, Domain: contracts.ErrorInfoDomain})
	if err != nil {
		return st // unreachable in practice: ErrorInfo always marshals
	}
	return withInfo
}

// Check returns nil when maxBytes is 0 or less (no limit) or virtualSize is
// at most maxBytes; a DiskSizeUnverifiableReason Refusal when a limit applies
// and virtualSize is unknown (0 or less, or readErr set); and a
// DiskSizeExceedsLimitReason Refusal when it is above the limit.
func Check(virtualSize int64, readErr error, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	if readErr != nil || virtualSize <= 0 {
		return &Refusal{reason: contracts.DiskSizeUnverifiableReason, message: UnverifiableMessage}
	}
	if virtualSize > maxBytes {
		return &Refusal{reason: contracts.DiskSizeExceedsLimitReason, message: ExceedsMessage}
	}
	return nil
}

// QemuImgVirtualSize parses the "virtual-size" of `qemu-img info
// --output=json` output; an output without a positive one is an error.
func QemuImgVirtualSize(stdout string) (int64, error) {
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return 0, fmt.Errorf("parse qemu-img info output: %w", err)
	}
	if info.VirtualSize <= 0 {
		return 0, fmt.Errorf("qemu-img info reported no virtual size")
	}
	return info.VirtualSize, nil
}

// LocalVirtualSize runs `qemu-img info -f format --output=json -- path` in
// the provider's own container (the format is pinned: nothing is probed) and
// returns the virtual size.
func LocalVirtualSize(ctx context.Context, path, format string) (int64, error) {
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "-f", format, "--output=json", "--", path).Output()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info: %w", err)
	}
	return QemuImgVirtualSize(string(out))
}
