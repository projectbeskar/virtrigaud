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

// Package fake is a deliberately instrumented clustered Provider for the
// conformance suite: an in-memory implementation of the provider gRPC service
// that obeys every rule of the contract, and that can be told to break any
// one of them (a Violation).
//
// It has two jobs. It is the reference the suite is first written against,
// and it is the oracle of the suite's own meta-test: for each Violation the
// meta-test runs the suite against a fake that breaks that one rule and checks
// that the cases that guard the rule fail. A conformance case that no
// Violation makes fail is a case that proves nothing.
package fake

// Violation names one contract rule the fake can be made to break.
type Violation string

// The violations. Each doc comment names the rule it breaks.
const (
	// AllowEmptyHost lets a call with no target_host_id default to the first
	// host instead of failing InvalidArgument.
	AllowEmptyHost Violation = "allow-empty-host"
	// UnknownHostNotFound answers a call to an unknown host NotFound (the VM
	// looks gone) instead of the host-scoped Unavailable.
	UnknownHostNotFound Violation = "unknown-host-not-found"
	// UnknownHostNoReason answers a call to an unknown host Unavailable with no
	// HOST_UNAVAILABLE ErrorInfo.
	UnknownHostNoReason Violation = "unknown-host-no-reason"
	// DialRequestedHostFirst connects to the host id named by the request
	// before checking that the provider fronts it.
	DialRequestedHostFirst Violation = "dial-requested-host-first"
	// NormalizeHostIDs routes a malformed host id (upper case, a path, a port)
	// to the host it resembles instead of refusing it.
	NormalizeHostIDs Violation = "normalize-host-ids"
	// FallbackToAnyHost looks a VM up on the other hosts when it is not on the
	// named one.
	FallbackToAnyHost Violation = "fallback-to-any-host"

	// IgnoreOwnerDescribe returns a foreign VM's state from Describe.
	IgnoreOwnerDescribe Violation = "ignore-owner-describe"
	// IgnoreOwnerMutate lets Power, Delete and Reconfigure act on a foreign VM.
	IgnoreOwnerMutate Violation = "ignore-owner-mutate"
	// UnstampedAuthorizes treats a VM with no owner stamp as anyone's.
	UnstampedAuthorizes Violation = "unstamped-authorizes"
	// MissingOwnerAuthorizes lets a request with no owner act on any VM.
	MissingOwnerAuthorizes Violation = "missing-owner-authorizes"
	// ForeignWrongCode refuses a foreign VM with PermissionDenied instead of
	// NotFound.
	ForeignWrongCode Violation = "foreign-wrong-code"
	// OwnerLeak names the other owner in the refusal message.
	OwnerLeak Violation = "owner-leak"
	// NotOwnedReasonMissing never attaches VM_NOT_OWNED although the fake
	// declares the feature.
	NotOwnedReasonMissing Violation = "not-owned-reason-missing"
	// NotOwnedReasonOnAbsent attaches VM_NOT_OWNED to a VM that does not exist.
	NotOwnedReasonOnAbsent Violation = "not-owned-reason-on-absent"

	// HostDownAsNotFound answers a call to an unreachable host NotFound.
	HostDownAsNotFound Violation = "host-down-as-not-found"
	// HostDownNoReason answers a call to an unreachable host Unavailable with
	// no HOST_UNAVAILABLE ErrorInfo.
	HostDownNoReason Violation = "host-down-no-reason"
	// HostUnavailableWrongDomain gives the HOST_UNAVAILABLE ErrorInfo another
	// domain.
	HostUnavailableWrongDomain Violation = "host-unavailable-wrong-domain"
	// HostUnavailableNoHost leaves the unreachable host out of the ErrorInfo
	// metadata.
	HostUnavailableNoHost Violation = "host-unavailable-no-host"
	// HostDownBreaksPeers makes every host fail while any one is unreachable.
	HostDownBreaksPeers Violation = "host-down-breaks-peers"
	// StickyUnreachable keeps refusing a host after it has recovered.
	StickyUnreachable Violation = "sticky-unreachable"
	// HostDownMutatesAnyway applies a power operation to an unreachable host
	// and then reports it unavailable.
	HostDownMutatesAnyway Violation = "host-down-mutates-anyway"

	// TaskNotHostEncoded returns plain task references.
	TaskNotHostEncoded Violation = "task-not-host-encoded"
	// TaskAcceptsMalformed reports any reference that is not host-encoded done.
	TaskAcceptsMalformed Violation = "task-accepts-malformed"
	// TaskUnknownHostDone reports a task of a host the provider does not front
	// done.
	TaskUnknownHostDone Violation = "task-unknown-host-done"
	// TaskDialsUnknownHost connects to the host a task reference names before
	// checking it.
	TaskDialsUnknownHost Violation = "task-dials-unknown-host"
	// TaskForgedInnerDone reports a task that was never issued done.
	TaskForgedInnerDone Violation = "task-forged-inner-done"
	// TaskWrongHost ignores the host in a task reference and asks the first
	// reachable host.
	TaskWrongHost Violation = "task-wrong-host"
	// TasksInMemory keeps the task table in the provider, so a restart loses it.
	TasksInMemory Violation = "tasks-in-memory"
	// PowerNoEffect reports a power operation done, with no task, without
	// applying it.
	PowerNoEffect Violation = "power-no-effect"

	// ListDropsUnreachable leaves an unreachable host out of ListVMs without
	// naming it.
	ListDropsUnreachable Violation = "list-drops-unreachable"
	// ListFailsOnDownHost fails the whole ListVMs when one host is unreachable.
	ListFailsOnDownHost Violation = "list-fails-on-down-host"
	// ListNoHostID leaves host_id empty on listed VMs.
	ListNoHostID Violation = "list-no-host-id"
	// ListDedupesByName lists one VM per name, whatever the host.
	ListDedupesByName Violation = "list-dedupes-by-name"

	// CapsNoClustering reports supports_clustering=false.
	CapsNoClustering Violation = "caps-no-clustering"
	// CapsUnstable changes the capabilities from call to call.
	CapsUnstable Violation = "caps-unstable"
	// CapsAdvertiseUnrouted advertises disk export and clone, which it does
	// not serve.
	CapsAdvertiseUnrouted Violation = "caps-advertise-unrouted"
	// NotReadyLooksHostScoped marks a Provider-level failure HOST_UNAVAILABLE.
	NotReadyLooksHostScoped Violation = "not-ready-looks-host-scoped"

	// DescribeFlaps makes Describe change the VM it reads.
	DescribeFlaps Violation = "describe-flaps"
	// PowerRepeatFails fails a power operation for a state the VM is in.
	PowerRepeatFails Violation = "power-repeat-fails"
	// DeleteTwiceFails fails Delete of a VM that is already gone with Internal
	// instead of NotFound.
	DeleteTwiceFails Violation = "delete-twice-fails"

	// ForgetsVMsOnRestart loses every VM when the provider restarts or its
	// server is served again (the provider kept the inventory in memory).
	ForgetsVMsOnRestart Violation = "forgets-vms-on-restart"
	// RestoresDeletedOnRestart brings a deleted VM back when the provider
	// restarts.
	RestoresDeletedOnRestart Violation = "restores-deleted-on-restart"
	// RestartDropsDownHost forgets an unreachable host when the provider
	// restarts, so ListVMs no longer reports it.
	RestartDropsDownHost Violation = "restart-drops-down-host"

	// InventoryDropsDownHost leaves an unreachable host out of ListHosts.
	InventoryDropsDownHost Violation = "inventory-drops-down-host"
	// InventoryDownIsReady reports an unreachable host Ready.
	InventoryDownIsReady Violation = "inventory-down-is-ready"
	// GetHostInfoUnknownOK answers GetHostInfo for a host it does not front.
	GetHostInfoUnknownOK Violation = "get-host-info-unknown-ok"

	// TaskDoneWithoutEffect reports a task done without having applied its
	// effect.
	TaskDoneWithoutEffect Violation = "task-done-without-effect"
	// NameMatchAuthorizes lets a request with no owner uid act on a VM whose
	// stamp records the same namespace and name.
	NameMatchAuthorizes Violation = "name-match-authorizes"
	// UnreadableStampAuthorizes acts on a VM whose owner stamp is unreadable
	// or ambiguous.
	UnreadableStampAuthorizes Violation = "unreadable-stamp-authorizes"
	// ForeignDescribeLeaksSize reports exists=false for a foreign VM but still
	// fills in its size.
	ForeignDescribeLeaksSize Violation = "foreign-describe-leaks-size"
	// BlackholeStallsList lets one host that never answers stall ListVMs
	// until the caller's deadline.
	BlackholeStallsList Violation = "blackhole-stalls-list"
	// BlackholeNoReason answers a call to a host that never answers with a
	// bare deadline error.
	BlackholeNoReason Violation = "blackhole-no-reason"
	// OpFailedNoReason answers an operation that failed on its host with a
	// plain Unavailable, without VM_OPERATION_FAILED.
	OpFailedNoReason Violation = "op-failed-no-reason"
	// ConnLostNoReason answers a lost host connection without
	// HOST_CONNECTION_LOST.
	ConnLostNoReason Violation = "conn-lost-no-reason"
	// RemovedHostIsNotFound answers a call to a host removed from the
	// registry NotFound, as if the VM were gone.
	RemovedHostIsNotFound Violation = "removed-host-is-not-found"

	// CreateIgnoresHost creates every VM on the first host.
	CreateIgnoresHost Violation = "create-ignores-host"
	// CreateIgnoresReachability creates a VM on an unreachable host.
	CreateIgnoresReachability Violation = "create-ignores-reachability"
	// CreateNoOwnerStamp creates VMs without an owner stamp.
	CreateNoOwnerStamp Violation = "create-no-owner-stamp"
	// CreateRebindsForeign binds a Create to a VM of the same name that
	// another owner made.
	CreateRebindsForeign Violation = "create-rebinds-foreign"
	// CreateDuplicates makes a second VM when a Create is repeated.
	CreateDuplicates Violation = "create-duplicates"
)

// All returns every violation, in declaration order.
func All() []Violation {
	return []Violation{
		AllowEmptyHost, UnknownHostNotFound, UnknownHostNoReason, DialRequestedHostFirst, NormalizeHostIDs, FallbackToAnyHost,
		IgnoreOwnerDescribe, IgnoreOwnerMutate, UnstampedAuthorizes, MissingOwnerAuthorizes, ForeignWrongCode, OwnerLeak,
		NotOwnedReasonMissing, NotOwnedReasonOnAbsent,
		HostDownAsNotFound, HostDownNoReason, HostUnavailableWrongDomain, HostUnavailableNoHost, HostDownBreaksPeers,
		StickyUnreachable, HostDownMutatesAnyway,
		TaskNotHostEncoded, TaskAcceptsMalformed, TaskUnknownHostDone, TaskDialsUnknownHost, TaskForgedInnerDone,
		TaskWrongHost, TasksInMemory, PowerNoEffect,
		ListDropsUnreachable, ListFailsOnDownHost, ListNoHostID, ListDedupesByName,
		CapsNoClustering, CapsUnstable, CapsAdvertiseUnrouted, NotReadyLooksHostScoped,
		DescribeFlaps, PowerRepeatFails, DeleteTwiceFails,
		ForgetsVMsOnRestart, RestoresDeletedOnRestart, RestartDropsDownHost,
		InventoryDropsDownHost, InventoryDownIsReady, GetHostInfoUnknownOK,
		TaskDoneWithoutEffect, NameMatchAuthorizes, UnreadableStampAuthorizes, ForeignDescribeLeaksSize, BlackholeStallsList,
		BlackholeNoReason, OpFailedNoReason, ConnLostNoReason, RemovedHostIsNotFound,
		CreateIgnoresHost, CreateIgnoresReachability, CreateNoOwnerStamp, CreateRebindsForeign, CreateDuplicates,
	}
}
