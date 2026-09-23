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

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/resources"
)

// The keys the per-phase durations are logged under. They are named like
// instruments but deliberately are not ones: the phases are implementation
// details of this binary, so they stay a developer-facing log record rather
// than becoming metric API (they decompose the ateom_restore /
// ateom_checkpoint phases of atelet's ate.actor.*.duration histograms, whose
// names they extend). Benchmarking tooling joins these records per actor;
// see benchmarking/analysis/.
const (
	restoreDurationKey    = "ateom.actor.restore.duration"
	checkpointDurationKey = "ateom.actor.checkpoint.duration"
	deltaSizeKey          = "ateom.snapshot.delta.size"
)

// phase is one timed step of a snapshot operation. A zero duration means the
// phase never ran (a Data-scope checkpoint captures no guest, a cold-run
// checkpoint merges nothing) and is skipped rather than logged as instant.
type phase struct {
	name string
	d    time.Duration
}

// scopeLogValue maps the ateom wire enum onto the shared scope label values,
// the same way ateattr.SnapshotScopeValue does for the atelet enum. An
// unrecognized scope reports as unknown rather than stringified, so no wire
// value can widen the value set readers key on.
func scopeLogValue(scope ateompb.SnapshotScope) string {
	switch scope {
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL:
		return ateattr.SnapshotScopeFull
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA:
		return ateattr.SnapshotScopeData
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA_ON_GOLDEN:
		return ateattr.SnapshotScopeDataOnGolden
	default:
		return ateattr.SnapshotScopeUnknown
	}
}

// logSnapshotPhases emits one joinable per-actor record for a checkpoint or
// restore, mirroring atelet's snapshotLogAttrs: full actor identity, the
// scope, and one float-seconds attr per non-zero phase under
// durationKey.<phase>. One record carries every phase of the operation, so a
// reader never joins two half-records that disagree about the same actor.
func logSnapshotPhases(ctx context.Context, msg string, a resources.ActorAttribution, scope ateompb.SnapshotScope, durationKey string, phases []phase, extra ...slog.Attr) {
	attrs := ateattr.ActorLogAttrs(a)
	attrs = append(attrs, slog.String(string(ateattr.SnapshotScopeKey), scopeLogValue(scope)))
	for _, p := range phases {
		if p.d == 0 {
			continue
		}
		// Seconds and not slog.Duration's nanoseconds: the values sit under
		// the atelet histograms' unit (s), so readers compare them without a
		// per-key unit table.
		attrs = append(attrs, slog.Float64(durationKey+"."+p.name, p.d.Seconds()))
	}
	attrs = append(attrs, extra...)
	slog.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
}
