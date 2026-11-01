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

package clustered_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// mutation is one seeded violation and the cases that must catch it.
type mutation struct {
	violation fake.Violation
	// sync runs the fake with synchronous calls.
	sync bool
	// kills are the IDs of the cases that must FAIL when the violation is
	// seeded. They are chosen from the rule the violation breaks, not from
	// what happens to fail.
	kills []string
	// want pins, for some of the kills, a substring the failure output must
	// contain, so the case is shown failing for the seeded reason and not for
	// another one.
	want map[string]string
}

func mutations() []mutation {
	return []mutation{
		{violation: fake.AllowEmptyHost, kills: []string{"route.missing-host", "create.requires-routable-host"}, want: map[string]string{"route.missing-host": `with host`}},
		{violation: fake.UnknownHostNotFound, kills: []string{"route.unknown-host", "errinfo.host-unavailable-wire-and-mapping", "create.requires-routable-host"}},
		{violation: fake.UnknownHostNoReason, kills: []string{"route.unknown-host", "errinfo.host-unavailable-wire-and-mapping"}},
		{violation: fake.DialRequestedHostFirst, kills: []string{"route.unknown-host", "route.invalid-host-id"}},
		{violation: fake.NormalizeHostIDs, kills: []string{"route.invalid-host-id"}},
		{violation: fake.FallbackToAnyHost, kills: []string{"route.correct-host"}, want: map[string]string{"route.correct-host": "does not have the VM"}},

		{violation: fake.IgnoreOwnerDescribe, kills: []string{"owner.describe-foreign", "owner.request-without-owner"}, want: map[string]string{"owner.describe-foreign": "must be exists=false"}},
		{violation: fake.IgnoreOwnerMutate, kills: []string{"owner.mutate-foreign", "owner.request-without-owner", "idempotency.delete"}, want: map[string]string{"owner.mutate-foreign": "must be refused"}},
		{violation: fake.UnstampedAuthorizes, kills: []string{"owner.describe-foreign", "owner.mutate-foreign"}},
		{violation: fake.MissingOwnerAuthorizes, kills: []string{"owner.request-without-owner"}},
		{violation: fake.ForeignWrongCode, kills: []string{"owner.mutate-foreign", "owner.refusal-discloses-nothing"}},
		{violation: fake.OwnerLeak, kills: []string{"owner.refusal-discloses-nothing"}, want: map[string]string{"owner.refusal-discloses-nothing": "names the other owner"}},
		{violation: fake.NotOwnedReasonMissing, kills: []string{"owner.vm-not-owned-reason"}},
		{violation: fake.NotOwnedReasonOnAbsent, kills: []string{"owner.vm-not-owned-reason"}},

		{violation: fake.HostDownAsNotFound, kills: []string{"hostdown.per-vm", "idempotency.host-unavailable", "errinfo.host-unavailable-wire-and-mapping"}, want: map[string]string{"hostdown.per-vm": "on an unreachable host"}},
		{violation: fake.HostDownNoReason, kills: []string{"hostdown.per-vm", "hostdown.not-counted-against-provider", "errinfo.host-unavailable-wire-and-mapping", "idempotency.host-unavailable", "tasks.host-down-ref", "restart.task-routing"}},
		{violation: fake.HostUnavailableWrongDomain, kills: []string{"hostdown.per-vm", "errinfo.host-unavailable-wire-and-mapping"}},
		{violation: fake.HostUnavailableNoHost, kills: []string{"hostdown.per-vm", "errinfo.host-unavailable-wire-and-mapping", "create.unavailable-names-host"}},
		{violation: fake.HostDownBreaksPeers, kills: []string{"hostdown.isolated"}},
		{violation: fake.StickyUnreachable, kills: []string{"hostdown.recovery", "list.recovery"}},
		{violation: fake.HostDownMutatesAnyway, kills: []string{"idempotency.host-unavailable"}},

		{violation: fake.TaskNotHostEncoded, kills: []string{"tasks.async-routing", "tasks.synchronous-effect"}, want: map[string]string{"tasks.async-routing": "must be host-encoded"}},
		{violation: fake.TaskAcceptsMalformed, kills: []string{"tasks.malformed-ref"}, want: map[string]string{"tasks.malformed-ref": "must not be reported as a task result"}},
		{violation: fake.TaskUnknownHostDone, kills: []string{"tasks.unknown-host"}, want: map[string]string{"tasks.unknown-host": "must be refused"}},
		{violation: fake.TaskDialsUnknownHost, kills: []string{"tasks.unknown-host"}},
		{violation: fake.TaskForgedInnerDone, kills: []string{"tasks.forged-inner-ref"}},
		{violation: fake.TaskWrongHost, kills: []string{"tasks.async-routing", "tasks.host-down-ref"}},
		{violation: fake.TasksInMemory, kills: []string{"restart.task-ref"}},
		{violation: fake.PowerNoEffect, sync: true, kills: []string{"tasks.synchronous-effect", "idempotency.power"}},

		{violation: fake.ListDropsUnreachable, kills: []string{"list.unreachable-host", "list.all-unreachable", "restart.host-down-across-restart"}, want: map[string]string{"list.unreachable-host": "the dead host is reported unknown"}},
		{violation: fake.ListFailsOnDownHost, kills: []string{"list.unreachable-host", "list.all-unreachable", "list.not-counted-against-provider"}},
		{violation: fake.ListNoHostID, kills: []string{"list.all-hosts"}, want: map[string]string{"list.all-hosts": "has no host_id"}},
		{violation: fake.ListDedupesByName, kills: []string{"list.all-hosts"}},

		{violation: fake.CapsNoClustering, kills: []string{"caps.clustering"}, want: map[string]string{"caps.clustering": "a clustered provider says so"}},
		{violation: fake.CapsUnstable, kills: []string{"caps.clustering"}},
		{violation: fake.CapsAdvertiseUnrouted, kills: []string{"caps.advertised-rpcs-are-routed"}},
		{violation: fake.NotReadyLooksHostScoped, kills: []string{"errinfo.provider-level-unavailable"}},

		{violation: fake.DescribeFlaps, kills: []string{"idempotency.reads"}},
		{violation: fake.PowerRepeatFails, kills: []string{"idempotency.power"}},
		{violation: fake.DeleteTwiceFails, kills: []string{"idempotency.delete"}},

		{violation: fake.ForgetsVMsOnRestart, kills: []string{"restart.state-lives-on-hosts", "providerdown.outage-and-recovery"}, want: map[string]string{"restart.state-lives-on-hosts": "still known on its host"}},
		{violation: fake.RestoresDeletedOnRestart, kills: []string{"restart.no-resurrection"}},
		{violation: fake.RestartDropsDownHost, kills: []string{"restart.host-down-across-restart", "restart.task-routing"}},

		{violation: fake.InventoryDropsDownHost, kills: []string{"hosts.inventory"}},
		{violation: fake.InventoryDownIsReady, kills: []string{"hosts.inventory"}},
		{violation: fake.GetHostInfoUnknownOK, kills: []string{"hosts.inventory"}},

		{violation: fake.TaskDoneWithoutEffect, kills: []string{"tasks.async-routing", "tasks.synchronous-effect", "idempotency.power"}, want: map[string]string{"tasks.async-routing": "has had its effect"}},
		{violation: fake.NameMatchAuthorizes, kills: []string{"owner.request-without-owner"}},
		{violation: fake.UnreadableStampAuthorizes, kills: []string{"owner.unreadable-stamp"}},
		{violation: fake.ForeignDescribeLeaksSize, kills: []string{"owner.describe-foreign"}, want: map[string]string{"owner.describe-foreign": "the answer must be empty"}},
		{violation: fake.BlackholeStallsList, kills: []string{"hostdown.blackholed-list"}},
		{violation: fake.BlackholeNoReason, kills: []string{"hostdown.blackholed-per-vm"}},
		{violation: fake.OpFailedNoReason, kills: []string{"hostdown.operation-failed"}, want: map[string]string{"hostdown.operation-failed": "must carry VM_OPERATION_FAILED"}},
		{violation: fake.ConnLostNoReason, kills: []string{"hostdown.connection-lost"}, want: map[string]string{"hostdown.connection-lost": "must carry HOST_CONNECTION_LOST"}},
		{violation: fake.RemovedHostIsNotFound, kills: []string{"hostdown.removed-host"}},
		{violation: fake.CreateIgnoresHost, kills: []string{"create.routed-to-target-host"}},
		{violation: fake.CreateIgnoresReachability, kills: []string{"create.requires-routable-host"}},
		{violation: fake.CreateNoOwnerStamp, kills: []string{"create.routed-to-target-host"}},
		{violation: fake.CreateRebindsForeign, kills: []string{"create.owner-rebind"}, want: map[string]string{"create.owner-rebind": "taken name"}},
		{violation: fake.CreateDuplicates, kills: []string{"create.owner-rebind"}},
	}
}

func caseByID(t *testing.T, id string) clustered.Case {
	t.Helper()
	for _, c := range clustered.Cases() {
		if c.ID == id {
			return c
		}
	}
	require.Failf(t, "unknown case", "no case %q", id)
	return clustered.Case{}
}

// TestEveryCaseCatchesItsSeededViolation is the suite's mutation test. For
// every violation the fake can commit it runs the cases that guard the broken
// rule against a fake that commits it, and requires each to FAIL. It then
// requires that every case of the suite is guarded by at least one violation
// and that every violation is used. It shows each case able to fail for the
// reason it states; it does not prove the cases complete.
func TestEveryCaseCatchesItsSeededViolation(t *testing.T) {
	killed := map[string][]fake.Violation{}
	used := map[fake.Violation]bool{}
	for _, m := range mutations() {
		used[m.violation] = true
		t.Run(string(m.violation), func(t *testing.T) {
			opts := []fake.Option{fake.WithViolations(m.violation)}
			if m.sync {
				opts = append(opts, fake.Synchronous())
			}
			factory := func(clustered.T) clustered.Target { return fake.New(opts...) }
			for _, id := range m.kills {
				c := caseByID(t, id)
				rec := clustered.NewRecorder(id)
				got := rec.RunRecorded(func(rt clustered.T) { clustered.RunCase(rt, factory, c) })
				switch got {
				case clustered.OutcomeFailed:
					killed[id] = append(killed[id], m.violation)
					if sub, ok := m.want[id]; ok {
						assert.Contains(t, strings.Join(rec.Failures(), " "), sub, "case %s failed against %s, but not for the seeded reason", id, m.violation)
					}
				case clustered.OutcomeSkipped:
					t.Errorf("case %s was skipped (%s), so it cannot catch %s", id, rec.SkipReason(), m.violation)
				default:
					t.Errorf("case %s PASSED against a provider that commits %s: it does not guard that rule", id, m.violation)
				}
			}
		})
	}

	for _, v := range fake.All() {
		assert.True(t, used[v], "violation %s is not exercised by any mutation", v)
	}
	var unguarded []string
	for _, c := range clustered.Cases() {
		if len(killed[c.ID]) == 0 {
			unguarded = append(unguarded, c.ID)
		}
	}
	assert.Empty(t, unguarded, "cases that no seeded violation makes fail")

	if testing.Verbose() {
		ids := make([]string, 0, len(killed))
		for id := range killed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			names := make([]string, len(killed[id]))
			for i, v := range killed[id] {
				names[i] = string(v)
			}
			t.Logf("%-46s caught: %s", id, strings.Join(names, ", "))
		}
	}
}

// TestRecorderSeesFailureAndSkip pins the recorder the mutation test relies
// on: a failed assertion is a failure, a skip a skip, and a clean run a pass.
func TestRecorderSeesFailureAndSkip(t *testing.T) {
	assert.Equal(t, clustered.OutcomePassed, clustered.NewRecorder("p").RunRecorded(func(clustered.T) {}))
	assert.Equal(t, clustered.OutcomeFailed, clustered.NewRecorder("f").RunRecorded(func(rt clustered.T) {
		rt.Errorf("boom")
	}))
	rec := clustered.NewRecorder("s")
	assert.Equal(t, clustered.OutcomeSkipped, rec.RunRecorded(func(rt clustered.T) { rt.Skipf("not today") }))
	assert.Equal(t, "not today", rec.SkipReason())
}

// TestReadmeListsEveryCase keeps test/conformance/README.md honest: every case
// of the suite and every optional feature is documented there, and the
// violation count it quotes is the real one.
func TestReadmeListsEveryCase(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	require.NoError(t, err)
	readme := string(b)
	for _, c := range clustered.Cases() {
		assert.True(t, strings.Contains(readme, "`"+c.ID+"`"), "case %s is not in test/conformance/README.md", c.ID)
		assert.NotEmpty(t, c.Source, "case %s cites no source", c.ID)
		assert.NotEmpty(t, c.Rule, "case %s states no rule", c.ID)
		for _, f := range c.Requires {
			assert.True(t, strings.Contains(readme, "`"+string(f)+"`"), "feature %s is not in test/conformance/README.md", f)
		}
	}
	assert.True(t, strings.Contains(readme, fmt.Sprintf("%d violations", len(fake.All()))), "the README quotes the number of violations (%d)", len(fake.All()))
}
