# libvirt: firmware, secure boot and platform features

This page describes the virtual platform the libvirt provider gives a new VM: machine type,
firmware (BIOS or UEFI, with or without Secure Boot), the emulated IOMMU and nested
virtualization. It also lists what the host needs, and what happens when libvirt refuses the
domain definition. The code is in `internal/providers/libvirt/domain_platform.go`.

## What a VMClass selects

| VMClass setting | Machine type | Firmware | Other |
|---|---|---|---|
| none of the settings below (the default) | `pc` (i440fx) | BIOS (SeaBIOS) | the domain XML is the same as in earlier releases |
| `firmware: UEFI` | `q35` | UEFI (OVMF) **without** Secure Boot | |
| `securityProfile.secureBoot: true` | `q35` | UEFI (OVMF) **with Secure Boot**, from a variable-store template with keys enrolled (see [Secure Boot trust model](#secure-boot-trust-model)) | SMM on. Applies whatever `firmware` says |
| `securityProfile.vtdEnabled: true` | `q35` | as selected by the rows above (BIOS by default) | emulated Intel IOMMU with interrupt remapping (`<iommu model='intel'><driver intremap='on'/></iommu>`, `<ioapic driver='qemu'/>`). The virtio devices do not go through it (see [Security notes](#security-notes)) |
| `securityProfile.tpmEnabled: true` | unchanged | unchanged | emulated TPM 2.0 (`tpm-tis`, swtpm) |
| `performanceProfile.nestedVirtualization: true` | unchanged | unchanged | `<cpu mode='host-passthrough' check='none'/>` |

- `firmware` is matched case-insensitively. `BIOS`, `EFI` and an unset value select BIOS. On
  libvirt, as on vSphere, only `UEFI` selects UEFI firmware.
- **Behaviour change in v0.4.1.** In earlier releases the libvirt provider ignored
  `firmware: UEFI`. A VMClass that sets it now creates **UEFI** VMs. Their image must boot with
  UEFI (see [The guest](#the-guest)), and the host needs an OVMF package. Existing VMs keep their
  domain definitions: the provider does not redefine them. To find the VMClasses this affects:
  `kubectl get vmclass -A -o jsonpath='{range .items[?(@.spec.firmware=="UEFI")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`.
- These settings take effect when a VM is created. A reconfigure does not change a VM's
  firmware or machine type.

## How the firmware is chosen

The provider never names a firmware file. A UEFI domain asks libvirt to select the firmware
from the descriptors that the host's OVMF package installs in `/usr/share/qemu/firmware/`.
Secure boot looks like this:

```xml
<os firmware='efi'>
  <type arch='x86_64' machine='q35'>hvm</type>
  <firmware>
    <feature enabled='yes' name='secure-boot'/>
    <feature enabled='yes' name='enrolled-keys'/>
  </firmware>
  <loader secure='yes'/>
  ...
</os>
<features>
  <acpi/>
  <apic/>
  <smm state='on'/>
</features>
```

`firmware: UEFI` without secure boot asks for `<feature enabled='no' name='secure-boot'/>`, which
selects an OVMF build that does not implement Secure Boot.

- **Enrolled keys.** Secure boot selects a variable-store template that has keys enrolled, so
  Secure Boot is enforced from the first boot. A template without enrolled keys starts in setup
  mode, and Secure Boot is then not enforced. libvirt does not fall back to such a template: a
  host without an enrolled one is refused. Earlier releases named a template without keys
  (`OVMF_VARS_4M.fd`) through a hard-coded Debian path.
- **The domain's variable store.** libvirt creates it from the selected template at the first
  start, at `/var/lib/libvirt/qemu/nvram/<domain>_VARS.fd`. libvirt uses a file that is already
  at that path as it is, so a new VM could start with the Secure Boot state of an earlier
  domain of the same name. After the define, the provider therefore reads the path from the new
  definition. It removes a file there that no other domain names as its store, and refuses the
  create (`Conflict`, the new domain is undefined again) when the path is a symbolic link or
  another domain's store. On libvirt releases that select the firmware only at start, the path
  is not known at create, and this check is not made.
- **Audit.** After the define, the provider logs the firmware libvirt selected: the loader, its
  `secure` flag, the variable-store template and the store path.
- **Delete.** Deleting the VM removes its own variable store (`virsh undefine --nvram`). A store
  that is not named after the domain, for example one named by an adopted or hand-made domain,
  is kept (`--keep-nvram`), and a `WARN` line names it. A BIOS domain is undefined as before. A
  clone gets its own copy of the store; see
  [libvirt-clones.md](libvirt-clones.md#clone-files-on-the-host).
- **When libvirt selects it.** libvirt 10.0 selects the firmware when the domain is defined.
  Then `virsh dumpxml` shows the resolved `<loader>` and `<nvram>` paths, and a host without a
  matching build is refused at create (see [Refusals](#when-libvirt-refuses-the-definition)).
  Older libvirt releases may select the firmware only when the domain starts. On those, the
  create succeeds and powering the VM on fails.
- **Before the copy.** Before a UEFI create copies the image, the provider asks the host
  (`virsh domcapabilities --machine q35`). When the answer lists no `efi` firmware, or no
  Secure Boot capable loader for a secure-boot VM, the create is refused (`InvalidSpec`)
  without copying anything. A host that cannot answer is not refused here; the define decides.

### Secure Boot trust model

Secure Boot proves only that the boot chain is signed by a key in the variable store's `db`.
Which keys those are depends on the OVMF package of the host. On Ubuntu 24.04 (`ovmf`
2024.02), `OVMF_VARS_4M.ms.fd` holds:

- the distribution's own platform key (PK), "Ubuntu OVMF Secure Boot (PK/KEK key)";
- Microsoft Corporation KEK CA 2011;
- in `db`, Microsoft Windows Production PCA 2011 and Microsoft Corporation UEFI CA 2011, the
  third-party CA.

So any boot chain that Microsoft's third-party CA signed boots, for example any vendor's signed
shim. It does not have to be your image's. The `dbx` revocation list is the one in the template
when the package was built. Later revocations reach a VM only when its guest applies `dbx`
updates, which are then kept in its own variable store. That template has no 2023 Microsoft
certificates. Media signed only with them do not boot until those certificates are enrolled. A
VMClass cannot choose another template in this release.

## The q35 device layout

A q35 domain does not use the i440fx device layout. libvirt refuses a q35 domain that has the
i440fx PCI root, an IDE controller or i440fx PCI slots ("The PCI controller with index='0' must
be model='pcie-root' for this machine type"). On q35:

- libvirt assigns every PCI address, and adds the `pcie-root` and the `pcie-root-port`s the
  devices need.
- The cloud-init seed CD-ROM is on SATA (`sda`), not IDE (`hda`). cloud-init finds the NoCloud
  seed by its volume label, so the bus makes no difference to the guest.
- USB is `qemu-xhci`, and video is standard VGA, which OVMF drives. There is no sound device.
- The primary disk is still `vda` (virtio), with the same serial console, VNC on `127.0.0.1`,
  guest-agent channel and memory balloon as on i440fx.

The i440fx layout is unchanged, with one fix. In earlier releases a VM with **more than one
network interface** could not be created: the second interface got PCI slot `0x04`, which the
sound card uses, and libvirt refused the domain ("Attempted double use of PCI Address"). Only
the first interface now has a fixed slot (`0x03`). libvirt assigns free slots to the others.

## Host prerequisites

| Feature | The host needs |
|---|---|
| UEFI or secure boot | a libvirt that supports the `<firmware><feature>` filters (7.2 or later; this release was tested on libvirt 10.0 with QEMU 8.2), a QEMU with the q35 machine type, and an OVMF package that installs firmware descriptors: `ovmf` (Debian, Ubuntu), `edk2-ovmf` (Fedora, RHEL and derivatives), `qemu-ovmf-x86_64` (SUSE) |
| Secure boot | a descriptor whose features include `secure-boot`, `enrolled-keys` and `requires-smm`. On Ubuntu 24.04, `ovmf` installs `40-edk2-x86_64-secure-enrolled.json` (`OVMF_CODE_4M.ms.fd` with `OVMF_VARS_4M.ms.fd`) |
| TPM | `swtpm` (and `swtpm-tools` on Debian and Ubuntu) |
| Nested virtualization | nesting enabled in the host's KVM module: `cat /sys/module/kvm_intel/parameters/nested` (or `kvm_amd`) prints `Y` or `1` |
| VT-d (emulated IOMMU) | nothing beyond QEMU with q35. The IOMMU is emulated by QEMU and does not depend on the host's own IOMMU |

To check a host:

```sh
ls /usr/share/qemu/firmware/
virsh domcapabilities --machine q35 --arch x86_64 | sed -n '/<os /,/<\/os>/p'
# expect <enum name='firmware'> with <value>efi</value>, and for secure boot
# <enum name='secure'> with <value>yes</value>
```

On a clustered Provider every host the scheduler can place a VM on needs these packages. A
refusal on the VM's pending host does not move the VM to another host: install the package on
that host, or delete the VM.

## The guest

- **UEFI.** The image must boot with UEFI: it needs an EFI system partition and a UEFI
  bootloader. Most current cloud images (Ubuntu, Debian, Fedora, RHEL and its derivatives) boot
  with both BIOS and UEFI. A BIOS-only image does not boot.
- **Secure boot.** The boot chain must be signed with keys the firmware trusts, for example the
  distribution's shim signed by Microsoft. An unsigned bootloader or kernel does not boot. On
  distributions that enforce module signatures under Secure Boot, unsigned kernel modules are
  refused too. In the guest, `mokutil --sb-state` prints `SecureBoot enabled`.
- **VT-d.** A Linux guest uses the IOMMU when booted with `intel_iommu=on`.
- **Nested virtualization.** With `host-passthrough` the guest sees the host CPU model and its
  VT-x or AMD-V. It sees them only when nesting is enabled on the host. Earlier releases
  required both `vmx` and `svm`, which no host CPU has, so such a VM failed to start ("Host CPU
  does not provide required features").

## Security notes

- **Nested virtualization** lets the guest run its own hypervisor on the host's KVM. That adds
  the nested VMX/SVM emulation to what a guest can reach, an area that has had guest-to-host
  vulnerabilities. With `host-passthrough`, the guest also sees the exact host CPU model and its
  flags. Anyone who can edit a VMClass can turn nesting on. The host-side control is the KVM
  module's `nested` parameter: set it to `0` on hosts that must not run nested guests.
- **VT-d** gives the guest an IOMMU (for example to assign devices inside a nested guest). The
  provider does not set `iommu='on'` on the virtio disk, network and other devices, so their
  DMA does not go through the emulated IOMMU. `vtdEnabled` is not DMA isolation of those
  devices.
- **The console.** The VNC console listens on the host's `127.0.0.1` without a password, as for
  every libvirt VM of this provider. Through the console, a user on the host can reach the
  firmware setup, the shim's MokManager and the boot loader's editor. There they can change or
  turn off Secure Boot. Access to the host is the trust boundary.
- **Firmware descriptors.** libvirt trusts the descriptors in `/etc/qemu/firmware/` and
  `/usr/share/qemu/firmware/` (a file in `/etc` overrides one of the same name). Keep those
  directories and the OVMF files as protected as the rest of the host.

## When libvirt refuses the definition

Some `virsh define` errors are libvirt refusing the definition itself: `XML error: ...`,
`unsupported configuration: ...`, or firmware selection finding no matching build ("Unable to
find 'efi' firmware that is compatible with the current configuration"). Some of these are
about the host rather than the VMClass, for example a missing OVMF build, or an emulator that
cannot run KVM guests. Only virsh's `error:` lines are read. After such an error, when the
provider has checked that the domain does not exist, the create is a definite refusal:

- The provider answers `InvalidArgument` (`InvalidSpec`). It does not answer a retryable
  failure. On single-host and clustered Providers alike, the `VirtualMachine` gets
  `Ready=False` and `Provisioning=False` with reason `ValidationError`. The message is
  "Provider rejected VM create (non-retryable; re-checking in <wait>, at <time>, or at once
  when the VirtualMachine, its VMClass, VMImage or Provider changes): libvirt refused the
  domain definition (the domain was not defined): <libvirt's reason>". libvirt's reason is
  shortened so that the provider's message fits the 256 bytes the manager shows.
- The disk that the attempt wrote (`<domain>-disk.qcow2`, or `<domain>-disk` for a blank disk) is
  removed again from the default pool, after a check that no domain uses it (on a clustered
  Provider, no domain on any host of the Provider). A migration's imported disk that the create
  attaches in place (`<domain>-migrated.qcow2`) is never removed.
- The manager knows that the attempt made nothing. Deleting the `VirtualMachine` is therefore
  not held as `CreateOutcomeUnknown`, and no `virtrigaud.io/create-outcome` annotation is
  needed.
- The manager sends the create again after 30 seconds, then 1, 2, 4 and 8 minutes, then every
  10 minutes while the `VirtualMachine` exists, and at once when the `VirtualMachine`, its
  `VMClass`, `VMImage` or `Provider` changes (see
  [A refused VM create is retried with a backoff](upgrading.md#a-refused-vm-create-is-retried-with-a-backoff)). When
  the host reports that it has no matching UEFI firmware, each attempt is refused before the
  copy (see [How the firmware is chosen](#how-the-firmware-is-chosen)). Any other refusal comes
  from the define, so each attempt copies the image, is refused, and removes the copy again.
  Fix the VMClass or install what the host is missing (see
  [Host prerequisites](#host-prerequisites)), or delete the VM.

The provider also removes the attempt's disk after any other create failure when the domain is
known not to be defined. That failure stays retryable. The disk is kept when the outcome of
`virsh define` cannot be established, because the domain may exist and use it.

The upgrade does not change a `VirtualMachine` whose create an earlier release answered as
retryable. Its create marker stays, because the answer to a re-sent create does not settle what
the first attempt did. Its delete can be held as `CreateOutcomeUnknown` until 10 minutes have
passed since the marker was written. The manager then releases it once the provider confirms
that every domain name the create could have used is absent (event `CreateConfirmedAbsent`).
The `virtrigaud.io/create-outcome` annotation that its message names releases it at once. A
disk left by such an attempt stays on the pool unless a later create of the same VM removes it.
Remove a leftover `<domain>-disk.qcow2` from the pool directory after you have checked that no
domain uses it.

## Known limitations

- **Memory snapshots of UEFI VMs.** libvirt (10.0) refuses an internal snapshot of a VM with
  pflash firmware: "internal snapshots of a VM with pflash based firmware are not supported".
  A `VMSnapshot` with `snapshotConfig.includeMemory: true` of a running UEFI VM is therefore
  not taken. Disk-only snapshots work. The UEFI variable store is not part of a disk-only
  snapshot, so a revert does not restore UEFI variables (for example the boot order).
- **One firmware per host.** libvirt selects the firmware on the host that the VM is created on.
  On a clustered Provider whose hosts have different OVMF packages, VMs of one VMClass can get
  different builds.
- **No live change.** Changing `firmware` or `securityProfile` on a VMClass does not change
  existing VMs.
