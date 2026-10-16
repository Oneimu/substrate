// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package scheduling decides which worker should host an actor.
package scheduling

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/workercache"
	"github.com/agent-substrate/substrate/internal/hardware"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/labels"
)

// Constraints describes what a worker must satisfy to host an actor.
type Constraints struct {
	// SandboxClass must equal the worker's sandbox class. Snapshots are not
	// portable across sandbox classes, so this is never relaxed.
	SandboxClass string

	// RequiredRuntime, when non-nil, restricts placement to workers whose
	// DefaultRuntime or RestorableRuntimes can restore a snapshot stamped with
	// this runtime.
	RequiredRuntime *ateapipb.SandboxRuntime

	// TemplateSelector and ActorSelector must both match the worker's labels.
	TemplateSelector labels.Selector
	ActorSelector    labels.Selector

	// RequiredNodes, when non-empty, restricts placement to workers running
	// on one of these nodes. Used when the actor's latest snapshot is local
	// to specific node VMs.
	RequiredNodes []string

	// Limits are the actor's declared resource limits, named as a Worker names
	// the capacity it reports, so the two subtract.
	Limits *ateapipb.Resources
}

// ErrNoCapacity is returned by Schedule when no worker that meets the
// constraints has room. The cause can be full workers, or no worker that meets
// the constraints at all.
var ErrNoCapacity = errors.New("no worker that meets the constraints has room")

// Scheduler answers placement questions against the current worker fleet.
type Scheduler interface {
	// Schedule returns a worker that meets constraints and has room.
	// Returns ErrNoCapacity when no worker that meets constraints has room.
	Schedule(ctx context.Context, constraints Constraints) (*ateapipb.Worker, error)

	// Applies reports whether worker satisfies non-capacity constraints. Capacity
	// is excluded so an existing assignment does not make its Worker ineligible.
	Applies(worker *ateapipb.Worker, constraints Constraints) bool

	// HasRoom reports whether worker's remaining capacity admits one more actor
	// of this size, in every dimension. Independent of Applies.
	HasRoom(worker *ateapipb.Worker, constraints Constraints) bool
}

// WorkerSource provides the whole fleet of workers, each with what the cache
// derived from its record when it ingested it.
type WorkerSource interface {
	Entries() ([]*workercache.Entry, error)
}

type scheduler struct {
	source WorkerSource
	// intn returns a uniformly distributed random value in [0,n).
	// Defaults to the global math/rand source
	intn func(n int) int
}

// Option configures the Scheduler returned by New.
type Option func(*scheduler)

// WithIntn overrides the random source used to sample candidate pairs: when
// n >= 2 candidates exist, Schedule calls intn(n) and then intn(n-1).
func WithIntn(intn func(n int) int) Option {
	return func(s *scheduler) { s.intn = intn }
}

// New returns a Scheduler placing onto workers reported by source.
func New(source WorkerSource, opts ...Option) Scheduler {
	s := &scheduler{source: source, intn: rand.Intn}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Schedule filters the fleet for eligible candidates with room, samples two at
// random (power of two choices), and returns the less-loaded one, where load is
// the higher of actor-slot and compute-resource utilization. Spreading across
// the warm pool avoids hotspots until autoscaling reclaims idle workers.
func (s *scheduler) Schedule(ctx context.Context, constraints Constraints) (*ateapipb.Worker, error) {
	entries, err := s.source.Entries()
	if err != nil {
		return nil, fmt.Errorf("while listing workers: %w", err)
	}

	// The actor's size is parsed once per pass. The workers' were parsed when
	// the cache ingested them, so the pass over the fleet parses nothing.
	need, err := workercache.ParseAmounts(constraints.Limits)
	if err != nil {
		return nil, fmt.Errorf("while parsing actor resource limits: %w", err)
	}

	var candidates []*workercache.Entry
	for _, e := range entries {
		if !s.Applies(e.Worker, constraints) {
			continue
		}
		if hasRoom(e, need) {
			candidates = append(candidates, e)
		}
	}

	if len(candidates) == 0 {
		return nil, ErrNoCapacity
	}
	if len(candidates) == 1 {
		return candidates[0].Worker, nil
	}

	i := s.intn(len(candidates))
	j := s.intn(len(candidates) - 1)
	if j >= i {
		j++
	}
	if lessLoaded(candidates[j], candidates[i]) {
		return candidates[j].Worker, nil
	}
	return candidates[i].Worker, nil
}

// lessLoaded compares dominant utilization — the higher of actor-slot
// utilization (allocated/capacity) and compute-resource utilization — so either
// dimension can mark a worker as hot. Ties fall back to actor-slot utilization,
// then to remaining actor slots.
func lessLoaded(a, b *workercache.Entry) bool {
	aAlloc := int64(a.Worker.GetStatus().GetAllocated().GetActors())
	bAlloc := int64(b.Worker.GetStatus().GetAllocated().GetActors())
	// hasRoom admits only workers with allocated < capacity, so capacity >= 1.
	aCap := int64(a.Worker.GetStatus().GetCapacity().GetActors())
	bCap := int64(b.Worker.GetStatus().GetCapacity().GetActors())

	aLoad := max(float64(aAlloc)/float64(aCap), a.Room.Utilization)
	bLoad := max(float64(bAlloc)/float64(bCap), b.Room.Utilization)
	if aLoad != bLoad {
		return aLoad < bLoad
	}

	// Compare slot utilization exactly via cross-multiplication.
	if lhs, rhs := aAlloc*bCap, bAlloc*aCap; lhs != rhs {
		return lhs < rhs
	}

	return aCap-aAlloc > bCap-bAlloc
}

func (s *scheduler) Applies(worker *ateapipb.Worker, constraints Constraints) bool {
	if worker.GetSandboxClass() != constraints.SandboxClass {
		return false
	}

	if worker.GetStatus().GetState() != ateapipb.WorkerState_WORKER_STATE_ACTIVE {
		return false
	}

	if len(constraints.RequiredNodes) > 0 && !slices.Contains(constraints.RequiredNodes, worker.GetNodeName()) {
		return false
	}

	if !hardware.WorkerMatches(worker.GetStatus(), constraints.RequiredRuntime) {
		return false
	}

	set := labels.Set(worker.GetLabels())
	if constraints.TemplateSelector != nil && !constraints.TemplateSelector.Matches(set) {
		return false
	}
	return constraints.ActorSelector == nil || constraints.ActorSelector.Matches(set)
}

// HasRoom reports whether what the worker has left admits one more actor of
// this size. A worker reports every dimension it has, so one it does not
// report is none of it, and an actor asking for that dimension does not fit.
//
// A worker whose recorded capacity or allocation will not parse is treated as
// having no room: it is the only answer that cannot overcommit a worker whose
// true occupancy is unreadable.
//
// This reads the worker's record directly, for a caller holding a row the
// cache has not seen. Schedule asks the same question of cache entries.
func (s *scheduler) HasRoom(worker *ateapipb.Worker, constraints Constraints) bool {
	need, err := workercache.ParseAmounts(constraints.Limits)
	if err != nil {
		return false
	}
	return hasRoom(workercache.NewEntry(worker), need)
}

// hasRoom is HasRoom for a worker the cache has read and an actor size already
// parsed, so Schedule can ask it of every worker without parsing anything.
func hasRoom(e *workercache.Entry, need workercache.Amounts) bool {
	capacity := e.Worker.GetStatus().GetCapacity()
	used := e.Worker.GetStatus().GetAllocated()

	// No per-actor size to compare: every assignment costs one, so a worker at
	// its limit has no room however small the next actor is.
	if used.GetActors() >= capacity.GetActors() {
		return false
	}
	if len(need) == 0 {
		return true
	}
	if e.Room.Unreadable {
		return false
	}
	for _, n := range need {
		// Both operands are copies, so a comparison that converts one to
		// inf.Dec converts the copy and leaves the shared Entry as it was.
		have := e.Room.Free.Get(n.Name) // absent is none of it
		if have.Cmp(n.Quantity) < 0 {
			return false
		}
	}
	return true
}
