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
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

const (
	testTemplateNamespace = "ate-agents"
	testTemplateName      = "support-agent"
)

// newTestInstruments builds the histograms against a local ManualReader so
// tests stay parallel-safe and never touch the global meter provider.
func newTestInstruments(t *testing.T) (*Instruments, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := NewInstruments(mp.Meter("ateom-microvm"))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	return inst, reader
}

func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not collected", name)
	return metricdata.Metrics{}
}

// phaseValues maps each recorded phase to the attribute set it carries.
func phaseValues(t *testing.T, m metricdata.Metrics) map[string]attribute.Set {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
	}
	byPhase := make(map[string]attribute.Set, len(hist.DataPoints))
	for _, dp := range hist.DataPoints {
		v, ok := dp.Attributes.Value(ateattr.SnapshotPhaseKey)
		if !ok {
			t.Errorf("datapoint without a phase attribute: %v", dp.Attributes.ToSlice())
			continue
		}
		byPhase[v.AsString()] = dp.Attributes
	}
	return byPhase
}

func attrString(t *testing.T, set attribute.Set, k attribute.Key) string {
	t.Helper()
	v, ok := set.Value(k)
	if !ok {
		t.Errorf("missing attribute %s in %v", k, set.ToSlice())
		return ""
	}
	return v.AsString()
}

func TestRecordRestoreShape(t *testing.T) {
	inst, reader := newTestInstruments(t)

	op := snapshotOp{
		templateNamespace: testTemplateNamespace,
		templateName:      testTemplateName,
		scope:             ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA_ON_GOLDEN,
	}
	inst.recordRestore(context.Background(), op,
		phase{ateattr.SnapshotPhaseVMRestore, 2 * time.Second},
		phase{ateattr.SnapshotPhaseReadyz, 500 * time.Millisecond},
		phase{ateattr.SnapshotPhaseTotal, 3 * time.Second})

	m := collectMetric(t, reader, restoreDurationMetric)
	if m.Unit != "s" {
		t.Errorf("unit = %q, want %q", m.Unit, "s")
	}
	if m.Description == "" {
		t.Error("description is empty")
	}

	byPhase := phaseValues(t, m)
	if len(byPhase) != 3 {
		t.Fatalf("recorded %d phases, want vm_restore, readyz and total", len(byPhase))
	}
	got := byPhase[ateattr.SnapshotPhaseVMRestore]
	for _, tc := range []struct {
		key  attribute.Key
		want string
	}{
		{ateattr.TemplateNamespaceKey, testTemplateNamespace},
		{ateattr.TemplateNameKey, testTemplateName},
		{ateattr.SnapshotScopeKey, ateattr.SnapshotScopeDataOnGolden},
	} {
		if v := attrString(t, got, tc.key); v != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, v, tc.want)
		}
	}
	if _, ok := got.Value(ateattr.ActorNameKey); ok {
		t.Error("actor identity must never reach a metric datapoint")
	}
	if _, ok := got.Value(ateattr.ActorUIDKey); ok {
		t.Error("actor identity must never reach a metric datapoint")
	}
}

// TestRecordCheckpointSkipsAbsentPhases keeps phases that never ran out of the
// percentiles: a Data-scope checkpoint captures no guest, a cold-run actor
// merges nothing, and reporting either as instantaneous would drag every
// percentile down.
func TestRecordCheckpointSkipsAbsentPhases(t *testing.T) {
	inst, reader := newTestInstruments(t)

	inst.recordCheckpoint(context.Background(), snapshotOp{
		templateNamespace: testTemplateNamespace,
		templateName:      testTemplateName,
		scope:             ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA,
	},
		phase{ateattr.SnapshotPhasePause, 5 * time.Millisecond},
		phase{ateattr.SnapshotPhaseSnapshot, 0},
		phase{ateattr.SnapshotPhaseRootfsUpper, 0},
		phase{ateattr.SnapshotPhaseMerge, 0},
		phase{ateattr.SnapshotPhaseDurableDir, 200 * time.Millisecond},
		phase{ateattr.SnapshotPhaseTotal, time.Second})

	byPhase := phaseValues(t, collectMetric(t, reader, checkpointDurationMetric))
	for _, absent := range []string{ateattr.SnapshotPhaseSnapshot, ateattr.SnapshotPhaseRootfsUpper, ateattr.SnapshotPhaseMerge} {
		if _, ok := byPhase[absent]; ok {
			t.Errorf("phase %q never ran but was recorded as a zero observation", absent)
		}
	}
	for _, present := range []string{ateattr.SnapshotPhasePause, ateattr.SnapshotPhaseDurableDir, ateattr.SnapshotPhaseTotal} {
		if _, ok := byPhase[present]; !ok {
			t.Errorf("phase %q missing", present)
		}
	}
	if v := attrString(t, byPhase[ateattr.SnapshotPhasePause], ateattr.SnapshotScopeKey); v != ateattr.SnapshotScopeData {
		t.Errorf("scope = %q, want %q", v, ateattr.SnapshotScopeData)
	}
}

func TestRecordDeltaSize(t *testing.T) {
	inst, reader := newTestInstruments(t)

	inst.recordDeltaSize(context.Background(), snapshotOp{
		templateNamespace: testTemplateNamespace,
		templateName:      testTemplateName,
		scope:             ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
	}, 82345)

	m := collectMetric(t, reader, deltaSizeMetric)
	if m.Unit != "By" {
		t.Errorf("unit = %q, want %q", m.Unit, "By")
	}
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 histogram", m.Name, m.Data)
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("recorded %d datapoints, want 1", len(hist.DataPoints))
	}
	dp := hist.DataPoints[0]
	if dp.Sum != 82345 {
		t.Errorf("sum = %d, want 82345", dp.Sum)
	}
	if v := attrString(t, dp.Attributes, ateattr.TemplateNamespaceKey); v != testTemplateNamespace {
		t.Errorf("template namespace = %q, want %q", v, testTemplateNamespace)
	}
	if v := attrString(t, dp.Attributes, ateattr.TemplateNameKey); v != testTemplateName {
		t.Errorf("template name = %q, want %q", v, testTemplateName)
	}
	// The delta is one file of one snapshot; the phase key names steps of an
	// operation and does not belong here.
	if _, ok := dp.Attributes.Value(ateattr.SnapshotPhaseKey); ok {
		t.Error("delta size must not carry a phase attribute")
	}
}

// TestScopeValue keeps the scope label bounded: every wire value maps onto the
// shared label set, and an unrecognized one reports as unknown rather than
// stringified.
func TestScopeValue(t *testing.T) {
	tests := []struct {
		scope ateompb.SnapshotScope
		want  string
	}{
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, ateattr.SnapshotScopeFull},
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA, ateattr.SnapshotScopeData},
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA_ON_GOLDEN, ateattr.SnapshotScopeDataOnGolden},
		{ateompb.SnapshotScope_SNAPSHOT_SCOPE_UNSPECIFIED, ateattr.SnapshotScopeUnknown},
		{ateompb.SnapshotScope(999), ateattr.SnapshotScopeUnknown},
	}
	for _, tt := range tests {
		if got := scopeValue(tt.scope); got != tt.want {
			t.Errorf("scopeValue(%v) = %q, want %q", tt.scope, got, tt.want)
		}
	}
}

// TestNilInstrumentsAreNoOps is the contract that lets AteomService run
// without a meter (hand-built in tests): every record helper must tolerate a
// nil receiver.
func TestNilInstrumentsAreNoOps(t *testing.T) {
	var inst *Instruments
	op := snapshotOp{scope: ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL}
	inst.recordRestore(context.Background(), op, phase{ateattr.SnapshotPhaseTotal, time.Second})
	inst.recordCheckpoint(context.Background(), op, phase{ateattr.SnapshotPhaseTotal, time.Second})
	inst.recordDeltaSize(context.Background(), op, 1)
}
