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

package proxmox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Imported-disk confinement (security).
//
// A migration to Proxmox stages the disk on the PVE node (ImportDisk) and the
// target VM's Create consumes it with `qm importdisk <vmid> <path>`, then
// removes the stage. The path reaches Create through
// VirtualMachine.spec.importedDisk.path, which tenants can write. Unconfined,
// any VirtualMachine could name any file or block device the SSH user can read
// on the node (another VM's disk, /etc/pve/priv/*): Create would copy it into
// the tenant's VM and then delete it.
//
// Create therefore takes the importdisk path only when ALL of these hold
// (checkImportedDiskPath), mirroring the vSphere (disk_guard.go) and libvirt
// (imagepath.go) providers:
//
//  1. contracts.VMImage.ImportedDisk is true: the manager verified the path as
//     the disk a VMMigration in the VM's own namespace imported for this VM
//     (isOwnMigrationDisk); tenants cannot set it.
//  2. The path is exactly the stage ImportDisk creates for this VM:
//     "/var/tmp/.virtrigaud-import-<ns>_<vm>-migrated.<qcow2|raw|vmdk>", <ns>
//     and <vm> being the requesting VirtualMachine's namespace
//     (CreateRequest.owner, required) and name (importStageName, derived from
//     ImportDiskRequest.target_vm). "_" appears in no Kubernetes name, so a
//     same-named VM in another namespace never gets, nor can name, this stage.
//     A stage an earlier release created under the bare name
//     ("/var/tmp/.virtrigaud-import-<vm>-migrated.<ext>") is refused with its
//     own message: that name does not separate namespaces (two same-named VMs
//     shared it), so it cannot be proven to be this VM's disk; that migration
//     must be run again (see legacyImportStagePath). The provider never
//     removes such a file: it is left for manual cleanup.
//  3. On the node, the stage is a regular file, not a symbolic link, with one
//     hard link (importStageNodeCheck). The check runs in the same shell
//     command as `qm importdisk`.
//
// Any other image Path is refused (InvalidArgument, nothing created) with a
// constant message that never echoes the path. A node file is only ever
// removed when its path is an import stage (isImportStagePath).

// importStageFilePrefix starts the file name of every import stage.
const importStageFilePrefix = ".virtrigaud-import-"

// importStageSeparator joins namespace and VM name in a stage name. It appears
// in no Kubernetes namespace or name, so "<ns>_<vm>" is unambiguous and no VM
// name equals a namespaced stage name.
const importStageSeparator = "_"

const (
	// maxImportStageNameBytes bounds the "<ns>_<vm>-migrated" part of a stage
	// file name, so prefix, name and extension stay well under the 255-byte
	// file name limit.
	maxImportStageNameBytes = 200
	// importStageHashMarker precedes the hash of a bounded stage name. An
	// unbounded name contains exactly one "_", so no unbounded name equals a
	// bounded one.
	importStageHashMarker = "__"
	// importStageHashHexLen is the number of hex digits (64 bits of SHA-256)
	// of a bounded stage name's hash.
	importStageHashHexLen = 16
	// importStageHashInputSeparator separates namespace and name in the hash
	// input; "/" appears in neither.
	importStageHashInputSeparator = "/"
	// importStagePrefixTrim are the characters trimmed from the end of a
	// bounded stage name's prefix before the marker.
	importStagePrefixTrim = "-._"
)

// importStageFormats are the stage file extensions ImportDisk writes (the
// staged object's source format).
var importStageFormats = []string{"qcow2", "raw", "vmdk"}

// importStagePathRE matches every node path ImportDisk stages a disk at: a
// plain file name "<ns>_<...>-migrated.<format>" in proxmoxImportStageDir (a
// DNS-label namespace, the separator, then the bounded or unbounded rest of
// the stage name). An earlier release's bare "<vm>-migrated" stages (no "_")
// do not match. cleanupNodeFile removes nothing else.
var importStagePathRE = regexp.MustCompile(`^` + regexp.QuoteMeta(strings.TrimRight(proxmoxImportStageDir, "/")+"/"+importStageFilePrefix) +
	`[a-z0-9][a-z0-9-]*` + importStageSeparator + `[a-z0-9._-]+` + regexp.QuoteMeta(contracts.ImportedDiskNameSuffix) +
	`\.(qcow2|raw|vmdk)$`)

// importedDiskRejectedMessage is the only text a refused imported-disk path
// returns. It names the rule, never the caller's path.
const importedDiskRejectedMessage = "imported disk rejected: the Proxmox provider imports a disk only when it is " +
	"the VirtualMachine's own migration stage (/var/tmp/.virtrigaud-import-<namespace>_<vm name>-migrated.<format>, " +
	"staged by a VMMigration in the same namespace that targets this VirtualMachine); use a VMImage (template) " +
	"for any other image"

// legacyImportStageMessage refuses a stage an earlier release created under
// the bare "<vm name>-migrated" name, which does not tell namespaces apart.
const legacyImportStageMessage = "imported disk rejected: it was staged by an earlier release under " +
	"/var/tmp/.virtrigaud-import-<vm name>-migrated.<format>, a name that does not separate namespaces, so it " +
	"cannot be proven to be this VirtualMachine's disk; delete this VirtualMachine and its VMMigration and run " +
	"the migration again (the old stage file is not removed automatically)"

// importTargetRequiredMessage refuses an ImportDisk without the target VM's
// identity (a manager older than the provider).
const importTargetRequiredMessage = "Proxmox disk import requires the target VirtualMachine's namespace and name " +
	"(ImportDiskRequest.target_vm); upgrade the manager"

// importedDiskFormatRejectedMessage is the only text a refused imported-disk
// format returns. It never echoes the requested value.
const importedDiskFormatRejectedMessage = "imported disk rejected: spec.importedDisk.format must be qcow2, raw or vmdk"

// importedDiskFormat returns the `qm importdisk --format` value (the TARGET
// format written into the VM's storage) for the image's Format field, which
// is tenant-writable (spec.importedDisk.format): absent or empty means "" (the
// caller's qcow2 default); one of importStageFormats is returned as is;
// anything else — another string or another JSON type — is refused with
// importedDiskFormatRejectedMessage.
func importedDiskFormat(raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	format, ok := raw.(string)
	if !ok {
		return "", errors.NewInvalidSpec("%s", importedDiskFormatRejectedMessage)
	}
	if format == "" || slices.Contains(importStageFormats, format) {
		return format, nil
	}
	return "", errors.NewInvalidSpec("%s", importedDiskFormatRejectedMessage)
}

// importedDiskRejected is the InvalidSpec refusal of an imported-disk path.
func importedDiskRejected() error {
	return errors.NewInvalidSpec("%s", importedDiskRejectedMessage)
}

// importStageName is the stage name of the disk a migration imports for the
// VirtualMachine namespace/name: "<ns>_<vm>-migrated". When that is longer
// than maxImportStageNameBytes it is bounded like the vSphere landing name:
// the start of "<ns>_<vm>", importStageHashMarker, the first
// importStageHashHexLen hex digits of SHA-256("<ns>/<vm>"), "-migrated".
func importStageName(namespace, name string) string {
	full := namespace + importStageSeparator + name + contracts.ImportedDiskNameSuffix
	if len(full) <= maxImportStageNameBytes {
		return full
	}
	sum := sha256.Sum256([]byte(namespace + importStageHashInputSeparator + name))
	tail := importStageHashMarker + hex.EncodeToString(sum[:])[:importStageHashHexLen] + contracts.ImportedDiskNameSuffix
	head := namespace + importStageSeparator + name
	return strings.TrimRight(head[:maxImportStageNameBytes-len(tail)], importStagePrefixTrim) + tail
}

// importStageFilePath is the node path of the stage named stageName in
// format: "/var/tmp/.virtrigaud-import-<stageName>.<format>".
func importStageFilePath(stageName, format string) string {
	return strings.TrimRight(proxmoxImportStageDir, "/") + "/" + importStageFilePrefix + stageName + "." + format
}

// legacyImportStagePath is where a release before this fix staged the disk
// for a VM named vmName, whatever its namespace:
// "/var/tmp/.virtrigaud-import-<vm>-migrated.<format>". Create refuses it
// (errLegacyImportStage) and never removes it; ImportDisk never creates it
// any more.
func legacyImportStagePath(vmName, format string) string {
	return importStageFilePath(vmName+contracts.ImportedDiskNameSuffix, format)
}

// validStageIdentity reports whether namespace/name can form a stage name: a
// DNS-1123 label namespace and a DNS-1123 subdomain name (no "/", no "..",
// no "_", lower case).
func validStageIdentity(namespace, name string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("namespace %q: %s", namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("name %q: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// importStageForRequest returns the stage name and node path of a migration
// import: importStageName over req.target_vm, in format. The bare
// target_name the manager also sends cannot tell namespaces apart and never
// decides the location; when set it must be the same VM's "<vm>-migrated". A
// request without target_vm (a manager older than the provider) is refused.
func importStageForRequest(req *providerv1.ImportDiskRequest, format string) (name, path string, err error) {
	tv := req.GetTargetVm()
	if tv.GetNamespace() == "" || tv.GetName() == "" {
		return "", "", errors.NewInvalidSpec("%s", importTargetRequiredMessage)
	}
	if err := validStageIdentity(tv.GetNamespace(), tv.GetName()); err != nil {
		return "", "", errors.NewInvalidSpec("import target cannot name a Proxmox import stage: %s", err.Error())
	}
	if req.GetTargetName() != "" && req.GetTargetName() != tv.GetName()+contracts.ImportedDiskNameSuffix {
		return "", "", errors.NewInvalidSpec("import target name %q does not match its target VM name %q",
			req.GetTargetName(), tv.GetName())
	}
	name = importStageName(tv.GetNamespace(), tv.GetName())
	return name, importStageFilePath(name, format), nil
}

// checkImportedDiskPath enforces rules 1 and 2 of the imported-disk
// confinement for the VM named vmName, requested by owner. diskPath is
// accepted only when imported is set, vmName is a valid VM name (and, when
// owner names one, the same), and diskPath is byte-for-byte this VM's
// namespaced stage (owner.Namespace required), in a stage format. Its legacy
// bare stage is refused with errLegacyImportStage; every other refusal is
// importedDiskRejected.
func checkImportedDiskPath(diskPath string, imported bool, owner contracts.ObjectIdentity, vmName string) error {
	if !imported || vmName == "" || (owner.Name != "" && owner.Name != vmName) {
		return importedDiskRejected()
	}
	if errs := validation.IsDNS1123Subdomain(vmName); len(errs) > 0 {
		return importedDiskRejected()
	}
	namespaced := owner.Namespace != "" && validStageIdentity(owner.Namespace, vmName) == nil
	for _, format := range importStageFormats {
		if namespaced && diskPath == importStageFilePath(importStageName(owner.Namespace, vmName), format) {
			return nil
		}
		if diskPath == legacyImportStagePath(vmName, format) {
			return errLegacyImportStage()
		}
	}
	return importedDiskRejected()
}

// errLegacyImportStage refuses a stage an earlier release created under the
// bare "<vm name>-migrated" name (InvalidSpec, as vSphere's errLegacyLanding).
func errLegacyImportStage() error {
	return errors.NewInvalidSpec("%s", legacyImportStageMessage)
}

// isImportStagePath reports whether path is a namespaced import stage path
// ("<ns>_<...>-migrated.<format>"): the only node files the provider's import
// code ever removes. A legacy bare-name stage does not match.
func isImportStagePath(path string) bool {
	return importStagePathRE.MatchString(path) && !strings.Contains(path, "..")
}

// importStageNodeCheck is the node-side shell condition a stage must meet
// before `qm importdisk` reads it (or a rebuild relies on it):
//   - a regular file (-f), not a symbolic link (-f alone follows one);
//   - exactly one hard link (not a second name of another file);
//   - owned by the user running the command (root on a PVE node: ImportDisk
//     writes every stage as that user), so no other account made or swapped it;
//   - no group or other write bit (mode & 022 == 0), so no other account could
//     have rewritten it after it was staged.
//
// A stat that fails fails the check. It is a free function so the exact text
// is unit-testable, and it is run against real files in the tests.
func importStageNodeCheck(stagePath string) string {
	q := proxmoxShellQuote(stagePath)
	return fmt.Sprintf(`[ -f %s ] && [ ! -L %s ] && [ "$(stat -c '%%h %%u' -- %s)" = "1 $(id -u)" ] && `+
		`[ $(( 0$(stat -c %%a -- %s) & 022 )) -eq 0 ]`, q, q, q, q)
}
