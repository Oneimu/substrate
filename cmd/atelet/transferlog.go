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
)

// The keys one file's transfer figures are logged under. Like ateom's phase
// keys they are named as if they were instruments but deliberately are not
// ones: which file a slow download or persist phase spent its time on is a
// developer-facing breakdown of the ate.actor.*.duration histograms, not
// workload-management data, so it stays a log record. Benchmarking tooling
// aggregates these; see benchmarking/analysis/.
const (
	transferDurationKey = "atelet.snapshot.transfer.duration"
	transferSizeKey     = "atelet.snapshot.transfer.size"
)

// The byte kinds one transfer reports, suffixed onto transferSizeKey.
const (
	bytesKindLogical   = "logical"
	bytesKindPopulated = "populated"
	bytesKindWire      = "wire"
)

// logTransfer emits one record for one snapshot file moving to (persist) or
// from (download) object storage: the duration, and up to three byte counts —
// logical (apparent size, holes included), populated (non-hole bytes actually
// read or written), and wire (compressed bytes on the network). Files move
// concurrently, so records within a phase compare against each other rather
// than summing to the phase total. A negative size means the route does not
// know that count and is skipped; zero is a real observation.
func logTransfer(ctx context.Context, templateAtespace, templateName, fileName, phaseName string, d time.Duration, logical, populated, wire int64) {
	attrs := []slog.Attr{
		slog.String(string(ateattr.TemplateAtespaceKey), templateAtespace),
		slog.String(string(ateattr.TemplateNameKey), templateName),
		slog.String(string(ateattr.SnapshotPhaseKey), phaseName),
		slog.String("file.name", fileName),
		slog.Float64(transferDurationKey, d.Seconds()),
	}
	for _, kind := range []struct {
		name string
		n    int64
	}{
		{bytesKindLogical, logical},
		{bytesKindPopulated, populated},
		{bytesKindWire, wire},
	} {
		if kind.n < 0 {
			continue
		}
		attrs = append(attrs, slog.Int64(transferSizeKey+"."+kind.name, kind.n))
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "Snapshot transfer breakdown", attrs...)
}
