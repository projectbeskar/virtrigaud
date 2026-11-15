/*
Copyright 2025.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostInUseFinalizer is the finalizer the Host controller keeps on every Host.
// It is removed only once no VirtualMachine of the
// Host's Provider names the Host in status.placement.host or
// status.placement.pendingHost, so a Host that per-VM calls (and VM finalizer
// cleanup) still route to cannot vanish from the provider's inventory.
const HostInUseFinalizer = "host.infra.virtrigaud.io/in-use"

// HostHealth is the live health of a Host, observed by the operator's inventory
// sync from the clustered provider (the ListHosts and GetHostInfo calls).
// +kubebuilder:validation:Enum=Ready;NotReady;Unknown
type HostHealth string

const (
	// HostHealthReady indicates the host is reachable and the provider could
	// read its inventory (for libvirt, a virsh nodeinfo over SSH succeeded).
	HostHealthReady HostHealth = "Ready"
	// HostHealthNotReady indicates the host was probed but is not currently
	// usable for placement or migration.
	HostHealthNotReady HostHealth = "NotReady"
	// HostHealthUnknown indicates the host's health has not yet been determined
	// (e.g. before the first successful heartbeat).
	HostHealthUnknown HostHealth = "Unknown"
)

// HostEndpointPattern is the exact set of connection URIs a Host.spec.endpoint
// may carry. It is the SINGLE regular expression shared by the CRD admission
// schema (the +kubebuilder:validation:Pattern marker on HostSpec.Endpoint, which
// must be kept byte-identical — TestHostCRDSchemaEndpointPatternMatchesGo
// enforces that) and the provider-side re-validation in
// internal/clustered/hostsecret, which cannot import this package.
//
// Accepted shapes (anything else is rejected):
//
//	qemu+ssh://[user@]host[:port]/system
//	qemu+ssh://[user@]host[:port]/session
//	grpc://host:port                      (passes validation; not connectable, see HostSpec.Endpoint)
//
// where user is [A-Za-z0-9._-]+ and host is a DNS name, an IPv4 address, or a
// bracketed IPv6 address. Query strings, fragments, percent-encoding, and any
// other path are rejected: the endpoint's path is forwarded to the hypervisor
// host as a libvirt connection URI, so it must never carry shell metacharacters
// (security: tenant-controlled command injection on the hypervisor host).
const HostEndpointPattern = `^(qemu\+ssh://([A-Za-z0-9._-]+@)?([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?/(system|session)|grpc://([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5})$`

// HostConditionAgentEnrolled is the Host condition type the host-agent
// enrolment controller sets on a Host whose Provider runs a per-host agent. It
// is True once a certificate for the host's agent has been validated and its
// key pinned, and False (with one of the HostReasonAgent* reasons) otherwise.
// Used only by development builds.
const HostConditionAgentEnrolled = "AgentEnrolled"

// Reasons of the AgentEnrolled Host condition. Used only by development
// builds.
const (
	// HostReasonAgentEnrolled: an issued certificate matched the authorised
	// request and its key is pinned.
	HostReasonAgentEnrolled = "Enrolled"
	// HostReasonAgentIdentityMissing: spec.identity is not set, so no
	// enrolment is authorised for the host.
	HostReasonAgentIdentityMissing = "IdentityMissing"
	// HostReasonAgentRevoked: spec.revoked is true, so no enrolment is
	// authorised for the host.
	HostReasonAgentRevoked = "Revoked"
	// HostReasonAgentAwaitingRequest: an enrolment record is drawn and the
	// manager waits for a certificate request carrying it.
	HostReasonAgentAwaitingRequest = "AwaitingCertificateRequest"
	// HostReasonAgentAwaitingCertificate: a certificate request is authorised
	// and the manager waits for the issued certificate.
	HostReasonAgentAwaitingCertificate = "AwaitingCertificate"
	// HostReasonAgentRequestDenied: the last certificate request for the host
	// was denied.
	HostReasonAgentRequestDenied = "CertificateRequestDenied"
	// HostReasonAgentCertificateRejected: an issued certificate did not match
	// the authorised request and was not pinned.
	HostReasonAgentCertificateRejected = "CertificateRejected"
	// HostReasonAgentCertificateExpired: the pinned agent certificate has
	// expired; the host is not rendered with agent credentials.
	HostReasonAgentCertificateExpired = "CertificateExpired"
)

// HostConditionAgentInventoryVerified is the Host condition type the Host
// controller sets on a host whose Provider runs a per-host agent: True when
// the last heartbeat got a verified signed host answer carrying a complete
// host inventory that is recent enough, False (with one of the
// HostReasonInventory* reasons) otherwise. Used only by development builds.
const HostConditionAgentInventoryVerified = "AgentInventoryVerified"

// HostConditionComponentsReady is the Host condition type the Host controller
// sets on a host whose Provider runs a per-host agent: True when the verified
// host inventory shows every host component ready and allow-listed, no
// unexpected system extension, every required feature present and an
// accepted process identity tier; False (with one of the HostReasonComponents*
// reasons) otherwise. A host is a placement candidate only while both this
// condition and AgentInventoryVerified are True. Used only by development
// builds.
const HostConditionComponentsReady = "ComponentsReady"

// Reasons of the AgentInventoryVerified Host condition. Used only by
// development builds.
const (
	// HostReasonInventoryVerified: the last heartbeat got a verified, complete
	// and recent host inventory.
	HostReasonInventoryVerified = "InventoryVerified"
	// HostReasonInventoryNoSignedAnswer: the last heartbeat got no verified
	// signed host answer (an error, an unsigned or stripped answer, a failed
	// verification, or an inventory that does not echo the request).
	HostReasonInventoryNoSignedAnswer = "NoSignedAnswer"
	// HostReasonInventoryMissing: the verified answer carries no inventory.
	HostReasonInventoryMissing = "InventoryMissing"
	// HostReasonInventoryInvalid: the host's agent signed an inventory that
	// breaks a field rule; the manager asks again after a backoff.
	HostReasonInventoryInvalid = "InventoryInvalid"
	// HostReasonInventoryPending: the agent has not finished its first
	// collection.
	HostReasonInventoryPending = "InventoryPending"
	// HostReasonInventoryStale: the collection the agent reported is older
	// than the maximum inventory age.
	HostReasonInventoryStale = "InventoryStale"
	// HostReasonInventoryOverLimit: the agent observed more than an
	// inventory can hold.
	HostReasonInventoryOverLimit = "InventoryOverLimit"
	// HostReasonInventoryUnreadable: the agent could not read its manifests
	// or finish its probes.
	HostReasonInventoryUnreadable = "InventoryUnreadable"
)

// Reasons of the ComponentsReady Host condition, in the order the manager
// checks them (the first that applies is reported). Used only by development
// builds.
const (
	// HostReasonComponentsVerified: every check passed.
	HostReasonComponentsVerified = "ComponentsVerified"
	// HostReasonComponentsInventoryNotVerified: AgentInventoryVerified is not
	// True.
	HostReasonComponentsInventoryNotVerified = "AgentInventoryNotVerified"
	// HostReasonComponentsAgentNotReady: the agent reports a state other than
	// ready.
	HostReasonComponentsAgentNotReady = "AgentNotReady"
	// HostReasonComponentsNoneReported: the verified inventory lists no host
	// component at all.
	HostReasonComponentsNoneReported = "NoComponentsReported"
	// HostReasonComponentsUnexpectedExtension: a system extension no
	// component declares is merged on the host.
	HostReasonComponentsUnexpectedExtension = "UnexpectedExtension"
	// HostReasonComponentsUnmanifested: a known component is present without
	// a manifest.
	HostReasonComponentsUnmanifested = "ComponentUnmanifested"
	// HostReasonComponentsManifestInvalid: a component manifest cannot be
	// used.
	HostReasonComponentsManifestInvalid = "ManifestInvalid"
	// HostReasonComponentsManifestOriginNotAccepted: a component is declared
	// by a manifest from a directory the manager does not accept.
	HostReasonComponentsManifestOriginNotAccepted = "ManifestOriginNotAccepted"
	// HostReasonComponentsMissing: a declared or required component is not
	// present.
	HostReasonComponentsMissing = "ComponentMissing"
	// HostReasonComponentsIncompatible: a component's version is outside the
	// range the agent requires.
	HostReasonComponentsIncompatible = "ComponentIncompatible"
	// HostReasonComponentsUnhealthy: a component failed a check on the host.
	HostReasonComponentsUnhealthy = "ComponentUnhealthy"
	// HostReasonComponentsAllowListNotConfigured: the manager has no
	// component allow-list. It is the reason of False, or of True when the
	// manager runs with the development-only flag that skips the allow-list.
	HostReasonComponentsAllowListNotConfigured = "AllowListNotConfigured"
	// HostReasonComponentsAllowListUnverified: the manager could not load the
	// component allow-list it was configured with (its digest differs from the
	// pinned one, or its signature does not verify).
	HostReasonComponentsAllowListUnverified = "AllowListUnverified"
	// HostReasonComponentsDigestNotAllowListed: a component's digests match
	// no allow-list entry for its version, or its version has no entry.
	HostReasonComponentsDigestNotAllowListed = "DigestNotAllowListed"
	// HostReasonComponentsDigestsNotReported: a component reports neither an
	// artifact digest nor files, so it cannot be checked.
	HostReasonComponentsDigestsNotReported = "DigestsNotReported"
	// HostReasonComponentsRequiredAbsent: a component the allow-list requires
	// is not reported ready.
	HostReasonComponentsRequiredAbsent = "RequiredComponentAbsent"
	// HostReasonComponentsFeatureMissing: a required feature probe is not
	// present.
	HostReasonComponentsFeatureMissing = "FeatureMissing"
	// HostReasonComponentsIdentityTierNotAccepted: the process identity tier
	// the agent reports is not accepted.
	HostReasonComponentsIdentityTierNotAccepted = "IdentityTierNotAccepted"
)

// HostEnrolVerb is the verb a requester must be allowed on the Host
// (resource hosts in group infra.virtrigaud.io, the Host's namespace and
// name) for the manager to approve an agent enrolment request for it. It is
// checked with a SubjectAccessReview of the request's recorded requester.
// Used only by development builds.
const HostEnrolVerb = "enrol"

// HostRekeyVerb is the verb a requester must be allowed on an enrolled Host
// for the manager to approve a re-key (a new enrolment of the same identity)
// of its agent, instead of HostEnrolVerb. Used only by development builds.
const HostRekeyVerb = "rekey"

// HostRekeyRequestAnnotation is the annotation that, set on an enrolled Host
// to an RFC 3339 time, asks the manager to open a re-key window for it: a
// record for its next enrolment is drawn and published until the window
// closes, an hour after that time. A value is used once. Used only by
// development builds.
const HostRekeyRequestAnnotation = "enrolment.virtrigaud.io/rekey-requested"

// HostAgentModeIdentity is the only agent enrolment mode: the agent proves its
// identity, and operation epochs carry no fencing meaning.
const HostAgentModeIdentity = "identity"

// HostSpec is admin-authored desired inventory: one bare hypervisor host that a
// clustered Provider (spec.topology=cluster) executes on. The admin
// declares which hosts exist; the provider reports what they can do right now
// (see HostStatus).
//
// spec.identity, once set, cannot be changed or removed (a replaced machine is
// a new Host identity and never inherits the old host's pinned keys), and
// spec.revoked, once true, cannot be cleared.
//
// Same-namespace model (security): a Host, its HostPool, its optional credential
// Secret, and the clustered Provider it names all live in ONE namespace — the
// Provider's. Every reference on this spec is therefore a LocalObjectReference
// (no namespace field). A Host in another namespace can neither enrol itself
// into a Provider's inventory nor point the operator at a credential Secret in
// a namespace its author does not control: the inventory render lists Hosts and
// resolves credential Secrets in the Provider's namespace only.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.identity) || (has(self.identity) && self.identity == oldSelf.identity)",message="spec.identity is immutable once set",fieldPath=".identity"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.revoked) || !oldSelf.revoked || (has(self.revoked) && self.revoked)",message="spec.revoked cannot be cleared once set; enrol a replacement as a new Host",fieldPath=".revoked"
type HostSpec struct {
	// ProviderRef is the clustered Provider that executes on this host. It must
	// be in the Host's own namespace: a Host can only be fronted by a Provider in
	// the same namespace (no cross-namespace inventory injection).
	ProviderRef LocalObjectReference `json:"providerRef"`

	// PoolRef is the HostPool this host belongs to. Membership is declared by the
	// host itself (Host.spec.poolRef names the HostPool), not by the pool.
	PoolRef LocalObjectReference `json:"poolRef"`

	// Endpoint is the host connection URI. Only these shapes are accepted:
	// qemu+ssh://[user@]host[:port]/system, qemu+ssh://[user@]host[:port]/session,
	// and grpc://host:port. The host is a DNS name, an IPv4 address, or a
	// bracketed IPv6 address. No query string, fragment, percent-encoding, or
	// other path is allowed.
	//
	// The only clustered provider type today is libvirt, and it connects to a
	// host over SSH with the qemu+ssh:// form only (the user, port and
	// system|session instance come from the URI; the SSH key and known_hosts
	// come from the credentials Secret). A grpc://host:port value passes
	// validation but the libvirt provider refuses to connect to it, so the host
	// cannot become Ready and is never used for placement; use qemu+ssh://.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^(qemu\+ssh://([A-Za-z0-9._-]+@)?([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?/(system|session)|grpc://([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*|\[[0-9A-Fa-f:.]+\]):[0-9]{1,5})$`
	Endpoint string `json:"endpoint"`

	// CredentialSecretRef optionally overrides the Provider's credentials for
	// this host (SSH key / known_hosts material). Defaults to the Provider's.
	// The Secret must be in the Host's (= the Provider's) namespace: the operator
	// never reads a credential Secret from another namespace on a Host's behalf,
	// so a Host author cannot exfiltrate a Secret they cannot already read.
	// +optional
	CredentialSecretRef *LocalObjectReference `json:"credentialSecretRef,omitempty"`

	// Labels are placement facts: storage-pool visibility, network/bridge
	// visibility, zone/rack, e.g. {"storage.virtrigaud.io/pool-nfs01":"true",
	// "net.virtrigaud.io/br-vlan100":"true","topology.virtrigaud.io/rack":"r7"}.
	// The scheduler treats them as hard constraints: a host is a candidate for a
	// VM only if it carries net.virtrigaud.io/<network>="true" for every
	// network the VM's VMNetworkAttachments name (<network> is the attachment's
	// libvirt networkName, or its bridge name when networkName is empty), and
	// the labels the VM's VMPlacementPolicy hard nodeSelector requires.
	// storage.virtrigaud.io/pool-<name> labels are accepted, but a VM create is
	// not checked against them.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// This is a defaulted bool and deliberately carries NO omitempty: with
	// omitempty an explicit false would be dropped on marshal, the apiserver
	// would re-apply the true default, and the field would silently flip
	// false->true.

	// Schedulable, when false, cordons the host: no new VM is placed on it, and
	// VMs already on it stay where they are (VirtRigaud does not move or
	// evacuate them). Defaults true.
	// +optional
	// +kubebuilder:default=true
	Schedulable bool `json:"schedulable"`

	// Identity is the host's identity anchor for per-host agent enrolment: for
	// example a TPM endorsement-key hash, a hardware serial number or an
	// administrator-assigned host UUID. It is set once and is immutable
	// afterwards. A host whose Provider runs a per-host agent can enrol only
	// once it is set; a replaced machine gets a new identity (a new Host) and
	// never inherits the old host's pinned keys. Used only by development
	// builds; ignored by libvirt hosts.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:-]*$`
	Identity string `json:"identity,omitempty"`

	// Revoked, when true, revokes the host's per-host agent identity: the
	// manager retires its enrolment and removes its pinned keys, never
	// authorises a certificate request for the host again, and does not
	// render the host into its Provider's inventory with agent credentials.
	// It cannot be cleared once set (enrol a replacement as a new Host). Used
	// only by development builds; ignored by libvirt hosts.
	// +optional
	Revoked bool `json:"revoked,omitempty"`
}

// HostAgentStatus is the per-host agent enrolment state the manager mirrors
// into Host.status.agent. It is a MIRROR: the authoritative record (the pinned
// keys and the enrolment record) lives in a manager-owned Secret per Host, and
// a lost or edited status is rewritten from it. It carries public identifiers
// only, never key material. Used only by development builds.
type HostAgentStatus struct {
	// Mode is the enrolment mode. Only "identity" exists.
	// +optional
	// +kubebuilder:validation:Enum=identity
	Mode string `json:"mode,omitempty"`

	// EnrolmentID is the current enrolment identifier: 32 lowercase
	// hexadecimal characters, carried in the agent certificate.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	EnrolmentID string `json:"enrolmentID,omitempty"`

	// PendingEnrolmentID is the enrolment identifier drawn for the next
	// enrolment, waiting for a certificate request that carries it.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	PendingEnrolmentID string `json:"pendingEnrolmentID,omitempty"`

	// RequestURIs are the exact URI subject alternative names a certificate
	// request for the pending enrolment must carry, and nothing else: the
	// host's URI identity and the enrolment record.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=512
	RequestURIs []string `json:"requestURIs,omitempty"`

	// KeyPins are the pinned agent keys: the SHA-256 of each certificate's
	// DER SubjectPublicKeyInfo, as 64 lowercase hexadecimal characters.
	// +optional
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:Pattern=`^[0-9a-f]{64}$`
	KeyPins []string `json:"keyPins,omitempty"`

	// CertificateExpiry is when the pinned agent certificate expires.
	// +optional
	CertificateExpiry *metav1.Time `json:"certificateExpiry,omitempty"`
}

// HostInventoryAllowList is the result of comparing one host component with
// the manager's component allow-list.
// +kubebuilder:validation:Enum=Allowed;NotAllowed;NotListed;NotChecked
type HostInventoryAllowList string

// Results of the allow-list comparison of a host component.
const (
	// HostInventoryAllowListAllowed: the component's artifact digest and
	// complete file set match an allow-list alternative for its version.
	HostInventoryAllowListAllowed HostInventoryAllowList = "Allowed"
	// HostInventoryAllowListNotAllowed: the allow-list has an entry for the
	// component and version but no alternative matches.
	HostInventoryAllowListNotAllowed HostInventoryAllowList = "NotAllowed"
	// HostInventoryAllowListNotListed: the allow-list has no entry for the
	// component and version.
	HostInventoryAllowListNotListed HostInventoryAllowList = "NotListed"
	// HostInventoryAllowListNotChecked: the component was not compared: it
	// reports no digest, or the manager has no allow-list.
	HostInventoryAllowListNotChecked HostInventoryAllowList = "NotChecked"
)

// HostInventoryFile is one file of a host component: a file the agent
// executes or loads, by name, with its digest.
type HostInventoryFile struct {
	// Name is the file's name within the component, never a path.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Digest is the SHA-256 of the file's content the agent computed, as
	// "sha256:" and 64 lowercase hexadecimal characters.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	Digest string `json:"digest"`
}

// HostInventoryComponent is one host component as the host's agent reported
// it, with the manager's allow-list result.
type HostInventoryComponent struct {
	// Name is the component name.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,62}$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// State is the component's readiness.
	// +kubebuilder:validation:Enum=Ready;Missing;Incompatible;Unhealthy;Unmanifested;ManifestInvalid
	State string `json:"state"`

	// Reason qualifies a state other than Ready, from a closed set.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason,omitempty"`

	// ObservedVersion is the version the agent read for the installed
	// component.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][0-9A-Za-z.+~_-]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	ObservedVersion string `json:"observedVersion,omitempty"`

	// DeclaredVersion is the version the component's manifest declares.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][0-9A-Za-z.+~_-]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	DeclaredVersion string `json:"declaredVersion,omitempty"`

	// RequiredRange is the version range the agent requires, set only for an
	// incompatible component.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z.+~_<>=, -]{1,64}$`
	// +kubebuilder:validation:MaxLength=64
	RequiredRange string `json:"requiredRange,omitempty"`

	// Source is how the manifest says the component was delivered.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	// +kubebuilder:validation:MaxLength=32
	Source string `json:"source,omitempty"`

	// ManifestOrigin is where the manifest that declares the component lies:
	// None, ReadOnly (the read-only image directory) or Writable (the
	// writable host-configuration directory).
	// +kubebuilder:validation:Enum=None;ReadOnly;Writable
	ManifestOrigin string `json:"manifestOrigin"`

	// ArtifactDigest is the SHA-256 of the artifact the component was
	// delivered in, as "sha256:" and 64 lowercase hexadecimal characters.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	ArtifactDigest string `json:"artifactDigest,omitempty"`

	// Files are the component's files the agent executes or loads.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Files []HostInventoryFile `json:"files,omitempty"`

	// AllowList is the result of the manager's allow-list comparison.
	AllowList HostInventoryAllowList `json:"allowList"`
}

// HostInventoryFeature is the result of one feature probe of the host's
// agent.
type HostInventoryFeature struct {
	// Name is the probe name, from a closed set.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Result is what the probe found.
	// +kubebuilder:validation:Enum=Present;Absent;Error
	Result string `json:"result"`

	// Reason qualifies an Error result, from a closed set.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason,omitempty"`
}

// HostInventoryExtension is a system extension merged on the host that no
// component declares.
type HostInventoryExtension struct {
	// Name is the extension's name, never a path.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name"`

	// Kind says how the extension is unexpected.
	// +kubebuilder:validation:Enum=Directory;UndeclaredImage;OverlaysComponent
	Kind string `json:"kind"`

	// ImageDigest is the SHA-256 of the extension image, as "sha256:" and 64
	// lowercase hexadecimal characters; empty for a directory extension.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	ImageDigest string `json:"imageDigest,omitempty"`
}

// HostAgentInventoryStatus is the host inventory the manager last verified
// in a signed host answer of the host's agent (see ObservedTime for when). It
// is cleared when the host's enrolment changes or its Provider no longer runs
// a per-host agent. It is an output only: no
// controller, the scheduler included, reads it back to decide anything; the
// conditions and health are computed from the verified answer itself. It is
// a statement by the host's agent, signed so the Provider cannot alter it,
// not a proof: a host whose agent is compromised can report anything. Used
// only by development builds.
type HostAgentInventoryStatus struct {
	// ObservedTime is when the manager last verified a host inventory of the
	// host.
	// +optional
	ObservedTime *metav1.Time `json:"observedTime,omitempty"`

	// AnswerSHA256 is the SHA-256 of the signed answer that carried the
	// inventory, for audit.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=64
	AnswerSHA256 string `json:"answerSHA256,omitempty"`

	// EnrolmentID is the enrolment of the host's agent whose answer carried
	// the inventory: 32 lowercase hexadecimal characters. The mirror is
	// cleared when the host's current enrolment differs.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	// +kubebuilder:validation:MaxLength=32
	EnrolmentID string `json:"enrolmentID,omitempty"`

	// AllowListSHA256 is the SHA-256 of the component allow-list the
	// manager compared the inventory with; empty when it has none.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=64
	AllowListSHA256 string `json:"allowListSHA256,omitempty"`

	// InventoryVersion is the version of the inventory content.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	InventoryVersion int64 `json:"inventoryVersion,omitempty"`

	// SupportedVersion is the highest inventory version the agent
	// implements.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	SupportedVersion int64 `json:"supportedVersion,omitempty"`

	// State says whether the inventory is complete.
	// +kubebuilder:validation:Enum=Complete;Pending;OverLimit;Unreadable
	State string `json:"state"`

	// StateReason qualifies an OverLimit or Unreadable state, from a closed
	// set.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]{0,63}$`
	// +kubebuilder:validation:MaxLength=64
	StateReason string `json:"stateReason,omitempty"`

	// SnapshotAgeSeconds is how long before its answer the agent finished
	// the collection it reported.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	SnapshotAgeSeconds int64 `json:"snapshotAgeSeconds,omitempty"`

	// ProcessIdentityTier is the strongest way the agent can identify the
	// processes it starts on the host.
	// +optional
	// +kubebuilder:validation:Enum=PidfdStore;PidfsHandle;PidfsInode;StartTime
	ProcessIdentityTier string `json:"processIdentityTier,omitempty"`

	// Components are the host components, sorted by name.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Components []HostInventoryComponent `json:"components,omitempty"`

	// Features are the feature probe results, sorted by name.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Features []HostInventoryFeature `json:"features,omitempty"`

	// UnexpectedExtensions are the system extensions merged on the host that
	// no component declares, sorted by name.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	UnexpectedExtensions []HostInventoryExtension `json:"unexpectedExtensions,omitempty"`
}

// HostStatus is operator-synced observed state from the provider's ListHosts and
// GetHostInfo calls. It carries capacity and health only, never connection
// secrets.
type HostStatus struct {
	// Health is the live host health as last observed by the inventory sync
	// (for libvirt, whether a virsh nodeinfo over SSH succeeded). Only Ready
	// hosts are placement candidates.
	// +optional
	Health HostHealth `json:"health,omitempty"`

	// AllocatableCPU is the schedulable vCPU capacity the provider reports live.
	// +optional
	AllocatableCPU *int32 `json:"allocatableCPU,omitempty"`

	// AllocatableMemoryMiB is the schedulable memory in MiB the provider reports
	// live.
	// +optional
	AllocatableMemoryMiB *int64 `json:"allocatableMemoryMiB,omitempty"`

	// AllocatableStorageBytes is the free storage in bytes the provider reports
	// live for new VM disks: for libvirt, the space available to non-root users
	// in the default storage pool's directory (statvfs, measured on every
	// sync), and 0 when it cannot be measured.
	// +optional
	AllocatableStorageBytes *int64 `json:"allocatableStorageBytes,omitempty"`

	// CPUModel is the host CPU baseline model as the provider reports it.
	// Informational: no controller reads it.
	// +optional
	CPUModel string `json:"cpuModel,omitempty"`

	// CPUFeatures are the host CPU features as the provider reports them. The
	// scheduler matches a VMPlacementPolicy's required CPU features against them.
	// +optional
	CPUFeatures []string `json:"cpuFeatures,omitempty"`

	// MachineTypes are the machine types the host supports, as the provider
	// reports them.
	// +optional
	MachineTypes []string `json:"machineTypes,omitempty"`

	// EmulatorVersion is the host emulator version as the provider reports it.
	// Informational: no controller reads it.
	// +optional
	EmulatorVersion string `json:"emulatorVersion,omitempty"`

	// BoundVMs is reserved for the count of VMs bound to this host. It is not
	// populated in this release and stays 0 (omitted); count the VirtualMachines
	// whose status.placement.host names this Host instead.
	// +optional
	BoundVMs int32 `json:"boundVMs,omitempty"`

	// Agent is the per-host agent enrolment state, mirrored from the
	// manager-owned pins Secret. Set only for hosts of a Provider that runs a
	// per-host agent. Used only by development builds.
	// +optional
	Agent *HostAgentStatus `json:"agent,omitempty"`

	// AgentInventory is the host inventory the manager last verified in a
	// signed answer of the host's agent, for operators and audit. An output
	// only: nothing reads it back to decide. Set only for hosts of a Provider
	// that runs a per-host agent. Used only by development builds.
	// +optional
	AgentInventory *HostAgentInventoryStatus `json:"agentInventory,omitempty"`

	// LastHeartbeatTime is when the operator last successfully synced this host.
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// ObservedGeneration reflects the generation of the Host spec observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the host's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=hvh
//+kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
//+kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef.name`
//+kubebuilder:printcolumn:name="Schedulable",type=boolean,JSONPath=`.spec.schedulable`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Host is the Schema for the hosts API: one bare hypervisor host registered into
// a clustered Provider's HostPool. It does not affect single-host
// (spec.topology=single) providers.
type Host struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostSpec   `json:"spec,omitempty"`
	Status HostStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HostList contains a list of Host.
type HostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Host `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Host{}, &HostList{})
}
