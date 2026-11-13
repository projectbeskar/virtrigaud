//go:build virtrigaud_dev

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

package fake

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
)

// The files a test uses to drive the fake executor from outside the agent
// process. They live in the executor's directory.
const (
	// ScriptFile holds the Script, as JSON. It is read at every step.
	ScriptFile = "script.json"
	// HoldFile, while it exists, holds every step before it starts: the
	// step waits (or ends at its deadline) until the file is removed. A
	// test uses it to keep an operation running.
	HoldFile = "hold"
	// EffectsLog has one line per effect the executor performed:
	// "<op_id hex> <step> <vm uid>".
	EffectsLog = "effects.log"
)

// pollEvery is how often a held or stalled step looks at the hold file. The
// fake is a development executor; its waits are bounded by the step's
// context.
const pollEvery = 10 * time.Millisecond

// Fault is what a scripted step does instead of its effect.
type Fault string

// The scriptable faults.
const (
	// FaultNotPerformed: the step does nothing and reports it definitely not
	// performed.
	FaultNotPerformed Fault = "not_performed"
	// FaultStall: the step does nothing and never returns on its own: it
	// ends when its context ends (the store's step deadline).
	FaultStall Fault = "stall"
	// FaultProbeError: the step does nothing and fails in a way that leaves
	// its effect in doubt, and the inspection of the step fails too: the
	// store cannot decide, and records the outcome unknown.
	FaultProbeError Fault = "probe_error"
	// FaultEffectThenError: the step performs its effect, then reports an
	// error that leaves the effect in doubt; inspection finds the effect.
	FaultEffectThenError Fault = "effect_then_error"
)

// ScriptedFault applies Fault to the next step that matches.
type ScriptedFault struct {
	// VMUID limits the fault to one VM; empty matches every VM.
	VMUID string `json:"vmUID,omitempty"`
	// Step limits the fault to one step name; empty matches every step.
	Step string `json:"step,omitempty"`
	// Fault is the fault.
	Fault Fault `json:"fault"`
}

// Script drives the fake executor. A test writes it with WriteScript.
type Script struct {
	// CreateStopped makes a Create leave the VM powered off.
	CreateStopped bool `json:"createStopped,omitempty"`
	// Faults are applied in order, each to the next step that matches; an
	// applied fault is removed from the script.
	Faults []ScriptedFault `json:"faults,omitempty"`
}

// errScripted is the error of a scripted step failure.
var errScripted = errors.New("scripted fault")

// WriteScript writes s into the fake executor directory dir, durably.
func WriteScript(dir string, s Script) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode fake executor script: %w", err)
	}
	return writeFile(filepath.Join(dir, ScriptFile), b)
}

// SetHold creates (on) or removes (off) the hold file in dir.
func SetHold(dir string, on bool) error {
	path := filepath.Join(dir, HoldFile)
	if on {
		return writeFile(path, nil)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove hold file: %w", err)
	}
	return nil
}

// Effect is one line of the effects log.
type Effect struct {
	// OpID is the operation's op_id in hex.
	OpID string
	// Step is the step name.
	Step string
	// VMUID is the VM.
	VMUID string
}

// Effects reads the effects log of the fake executor directory dir.
func Effects(dir string) ([]Effect, error) {
	f, err := os.Open(filepath.Join(dir, EffectsLog)) // #nosec G304 -- a path the caller owns
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read effects log: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []Effect
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) != 3 {
			continue
		}
		out = append(out, Effect{OpID: parts[0], Step: parts[1], VMUID: parts[2]})
	}
	return out, sc.Err()
}

// readScript reads the script; a missing file is the empty script.
func (e *Executor) readScript() (Script, error) {
	b, err := os.ReadFile(filepath.Join(e.dir, ScriptFile)) // #nosec G304 -- a path the executor built
	if errors.Is(err, fs.ErrNotExist) {
		return Script{}, nil
	}
	if err != nil {
		return Script{}, fmt.Errorf("read fake executor script: %w", err)
	}
	var s Script
	if err := json.Unmarshal(b, &s); err != nil {
		return Script{}, fmt.Errorf("decode fake executor script: %w", err)
	}
	return s, nil
}

// takeFault removes and returns the first fault that matches op and step.
// The caller holds e.mu.
func (e *Executor) takeFault(op executor.Operation, step executor.Step) (Fault, Script, error) {
	s, err := e.readScript()
	if err != nil {
		return "", s, err
	}
	for i, f := range s.Faults {
		if (f.VMUID == "" || f.VMUID == op.VMUID) && (f.Step == "" || f.Step == step.Name) {
			s.Faults = append(s.Faults[:i:i], s.Faults[i+1:]...)
			if err := WriteScript(e.dir, s); err != nil {
				return "", s, err
			}
			return f.Fault, s, nil
		}
	}
	return "", s, nil
}

// waitHold waits while the hold file exists, until ctx ends.
func (e *Executor) waitHold(ctx context.Context) error {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		held, err := fileExists(filepath.Join(e.dir, HoldFile))
		if err != nil {
			return err
		}
		if !held {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// logEffect appends one effect line, durably. The caller holds e.mu.
func (e *Executor) logEffect(op executor.Operation, step executor.Step) error {
	f, err := os.OpenFile(filepath.Join(e.dir, EffectsLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- a path the executor built
	if err != nil {
		return fmt.Errorf("open effects log: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%s %s %s\n", hex.EncodeToString(op.OpID), step.Name, op.VMUID); err != nil {
		_ = f.Close()
		return fmt.Errorf("write effects log: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync effects log: %w", err)
	}
	return f.Close()
}
