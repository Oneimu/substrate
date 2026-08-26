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
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

const (
	restoreDurationMetric    = "ateom.actor.restore.duration"
	checkpointDurationMetric = "ateom.actor.checkpoint.duration"
	deltaSizeMetric          = "ateom.snapshot.delta.size"
)

// snapshotPhaseBuckets match atelet's ate.actor.*.duration buckets, so the
// finer ateom phases recorded here line up under the ateom_restore /
// ateom_checkpoint phases they decompose.
var snapshotPhaseBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60}

// deltaSizeBuckets cover an OnDemand delta: from a near-idle actor that
// faulted a few hundred KiB back in, up to a guest that re-touched most of a
// multi-GiB working set.
var deltaSizeBuckets = []float64{1e5, 1e6, 5e6, 1e7, 2.5e7, 5e7, 1e8, 2.5e8, 5e8, 1e9, 2e9, 5e9}

// Instruments holds ateom's snapshot histograms: the checkpoint/restore phase
// breakdowns that previously existed only as structured log lines, and the
// OnDemand delta size. A nil *Instruments is a valid no-op, so call sites
// need no guard.
type Instruments struct {
	restoreDuration    metric.Float64Histogram
	checkpointDuration metric.Float64Histogram
	deltaSize          metric.Int64Histogram
}

func NewInstruments(meter metric.Meter) (*Instruments, error) {
	restoreDuration, err := meter.Float64Histogram(
		restoreDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of an actor restore inside ateom-microvm. The phases are sequential and partition the total. Recorded on success."),
		metric.WithExplicitBucketBoundaries(snapshotPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", restoreDurationMetric, err)
	}

	checkpointDuration, err := meter.Float64Histogram(
		checkpointDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of an actor checkpoint inside ateom-microvm. The snapshot, durable_dir and rootfs_upper captures run concurrently on the paused guest, so those phases are independent observations rather than a partition of the total. Recorded on success."),
		metric.WithExplicitBucketBoundaries(snapshotPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", checkpointDurationMetric, err)
	}

	deltaSize, err := meter.Int64Histogram(
		deltaSizeMetric,
		metric.WithUnit("By"),
		metric.WithDescription("Populated bytes of the OnDemand delta a checkpoint merged into its restore source: the pages the guest faulted in since the restore. Recorded only when a merge happened."),
		metric.WithExplicitBucketBoundaries(deltaSizeBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", deltaSizeMetric, err)
	}

	return &Instruments{
		restoreDuration:    restoreDuration,
		checkpointDuration: checkpointDuration,
		deltaSize:          deltaSize,
	}, nil
}

// snapshotOp is the dimension set shared by every phase of one restore or
// checkpoint. The sandbox class is not a dimension: everything this binary
// emits is microvm, and the service resource already says so.
type snapshotOp struct {
	templateNamespace string
	templateName      string
	scope             ateompb.SnapshotScope
}

func (o snapshotOp) attrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		ateattr.TemplateNamespaceKey.String(o.templateNamespace),
		ateattr.TemplateNameKey.String(o.templateName),
		ateattr.SnapshotScopeKey.String(scopeValue(o.scope)),
	}
}

// scopeValue maps the ateom wire enum onto the shared scope label values, the
// same way ateattr.SnapshotScopeValue does for the atelet enum. An
// unrecognized scope reports as unknown rather than stringified, so no wire
// value can widen the label set.
func scopeValue(scope ateompb.SnapshotScope) string {
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

// phase is one timed step of a snapshot operation.
type phase struct {
	name string
	d    time.Duration
}

func (i *Instruments) recordRestore(ctx context.Context, op snapshotOp, phases ...phase) {
	if i == nil || i.restoreDuration == nil {
		return
	}
	recordPhases(ctx, i.restoreDuration, op, phases)
}

func (i *Instruments) recordCheckpoint(ctx context.Context, op snapshotOp, phases ...phase) {
	if i == nil || i.checkpointDuration == nil {
		return
	}
	recordPhases(ctx, i.checkpointDuration, op, phases)
}

// recordDeltaSize reports the populated bytes of the OnDemand delta a
// checkpoint overlaid onto its restore source. Zero is a real observation (an
// idle actor faulted nothing back in); the caller skips the call entirely
// when no merge happened.
func (i *Instruments) recordDeltaSize(ctx context.Context, op snapshotOp, bytes int64) {
	if i == nil || i.deltaSize == nil {
		return
	}
	i.deltaSize.Record(ctx, bytes, metric.WithAttributes(
		ateattr.TemplateNamespaceKey.String(op.templateNamespace),
		ateattr.TemplateNameKey.String(op.templateName),
	))
}

// recordPhases skips zero-valued phases: those never ran (a Data-scope
// checkpoint captures no guest, a cold-run checkpoint merges nothing), and
// reporting them as instantaneous would drag every percentile down.
func recordPhases(ctx context.Context, h metric.Float64Histogram, op snapshotOp, phases []phase) {
	base := op.attrs()
	for _, p := range phases {
		if p.d == 0 {
			continue
		}
		attrs := make([]attribute.KeyValue, 0, len(base)+1)
		attrs = append(attrs, base...)
		attrs = append(attrs, ateattr.SnapshotPhaseKey.String(p.name))
		h.Record(ctx, p.d.Seconds(), metric.WithAttributes(attrs...))
	}
}
