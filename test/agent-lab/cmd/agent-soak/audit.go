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

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	vAuditLost    = "audit_event_never_emitted"
	vAuditPhantom = "audit_event_emitted_but_not_in_store"
)

// auditEv identifies an audit event across the log and the store.
type auditEv struct {
	seq       uint64
	event, op string
}

// auditReport compares the agent's audit lines (all runs) with the audit
// events the store holds.
type auditReport struct {
	StoreEvents       int `json:"store_events"`
	EmittedLive       int `json:"emitted_live"`
	Replayed          int `json:"replayed_lines"`
	ReplayedAlsoLive  int `json:"replayed_that_were_also_emitted_live"`
	ReplayedRecovered int `json:"replayed_never_emitted_live_recovered_after_crash"`
	NotEmittedAtAll   int `json:"store_events_never_emitted_or_replayed"`
	EmittedNotInStore int `json:"emitted_live_but_not_in_store"`
	ReplayCappedWarns int `json:"audit_replay_capped_lines"`
	LinesWithoutSeq   int `json:"event_lines_without_seq"`
}

// analyzeAudit reads the agent logs of runs 1..gens and the dump of the
// store's audit events. An event the store holds must have reached the log:
// live, or replayed after a restart. An event must not be emitted before it
// is in the store.
func (s *soak) analyzeAudit(dumpPath string, gens int) (auditReport, error) {
	var rep auditReport
	store := map[auditEv]bool{}
	f, err := os.Open(dumpPath) // #nosec G304 -- a path the lab chose
	if err != nil {
		return rep, fmt.Errorf("open audit dump: %w", err)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e auditEv
		var idx int
		var op string
		if _, err := fmt.Sscanf(sc.Text(), "%d %d %s %s", &e.seq, &idx, &e.event, &op); err != nil {
			continue
		}
		e.op = op
		store[e] = true
	}
	_ = f.Close()
	rep.StoreEvents = len(store)
	live, rep2 := map[auditEv]bool{}, map[auditEv]bool{}
	for g := 1; g <= gens; g++ {
		lf, err := os.Open(s.lab.Dirs.agentLog(g)) // #nosec G304 -- the lab's log
		if err != nil {
			continue
		}
		lsc := bufio.NewScanner(lf)
		lsc.Buffer(make([]byte, 1<<20), 1<<20)
		for lsc.Scan() {
			line := lsc.Text()
			if strings.Contains(line, "audit replay capped") {
				rep.ReplayCappedWarns++
			}
			if !strings.Contains(line, `"msg":"operation event"`) {
				continue
			}
			var j struct {
				Seq      uint64 `json:"seq"`
				Event    string `json:"event"`
				Op       string `json:"op"`
				Replayed bool   `json:"replayed"`
			}
			if json.Unmarshal([]byte(line), &j) != nil {
				continue
			}
			if j.Seq == 0 {
				rep.LinesWithoutSeq++
				continue
			}
			e := auditEv{j.Seq, j.Event, j.Op}
			if j.Replayed {
				rep.Replayed++
				rep2[e] = true
			} else {
				live[e] = true
			}
		}
		_ = lf.Close()
	}
	rep.EmittedLive = len(live)
	for e := range rep2 {
		if live[e] {
			rep.ReplayedAlsoLive++
		}
	}
	for e := range store {
		switch {
		case live[e]:
		case rep2[e]:
			rep.ReplayedRecovered++
		default:
			rep.NotEmittedAtAll++
			if rep.NotEmittedAtAll <= 5 {
				s.led.violate(vAuditLost, "store event seq %d %s op %s was never emitted nor replayed", e.seq, e.event, e.op)
			}
		}
	}
	for e := range live {
		if !store[e] {
			rep.EmittedNotInStore++
			if rep.EmittedNotInStore <= 5 {
				s.led.violate(vAuditPhantom, "event seq %d %s op %s was emitted but is not in the store", e.seq, e.event, e.op)
			}
		}
	}
	return rep, nil
}
