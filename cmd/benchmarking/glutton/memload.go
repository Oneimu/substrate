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
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// memLoad gives glutton a self-driving resident working set, so large-memory
// suspend/resume benchmarks don't depend on an external driver reaching the
// gRPC API (WriteRAM is unreachable in --mode=http deployments). The load
// allocates --mem-target bytes and then keeps re-writing them on a cycle:
// pages that are merely allocated but never touched don't stay resident and
// never reach a memory snapshot, so continuous dirtying is what makes the
// actor look like a real 1-2 GiB application to checkpoint/restore. The
// sweeper goroutine survives suspend/resume with the actor, so pages are
// re-dirtied between benchmark cycles too.
type memLoad struct {
	chunks  [][]byte
	touched atomic.Int64 // pages written since start, exported as a counter
	sweeps  atomic.Int64
}

// memChunkBytes bounds single allocations so the target size doesn't have to
// fit one contiguous region.
const memChunkBytes = 64 << 20

// pageBytes is the stride between writes. 4KiB matches the guest/host page
// size everywhere we run; writing one byte per page dirties the whole page.
const pageBytes = 4096

// startMemLoad allocates target bytes, dirties all of it once, and then keeps
// re-dirtying it: one full pass per interval ("sequential"), or the same
// write rate spread over uniformly random pages ("random"). It returns after
// the initial full dirtying pass, so a caller that waits for readyz after
// this knows the working set is resident.
func startMemLoad(ctx context.Context, target int64, interval time.Duration, pattern string) (*memLoad, error) {
	if target <= 0 {
		return nil, fmt.Errorf("mem-target must be positive, got %d", target)
	}
	switch pattern {
	case "sequential", "random":
	default:
		return nil, fmt.Errorf("mem-pattern must be sequential or random, got %q", pattern)
	}

	m := &memLoad{}
	for remaining := target; remaining > 0; remaining -= memChunkBytes {
		size := int64(memChunkBytes)
		if remaining < size {
			size = remaining
		}
		m.chunks = append(m.chunks, make([]byte, size))
	}

	// Initial pass: fault every page in so the working set is resident
	// before the actor reports ready.
	m.writePass(1, nil)

	meter := otel.Meter(meterName)
	if _, err := meter.Int64ObservableGauge("glutton.memload.target_bytes",
		metric.WithDescription("Configured self-driving resident working set size."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(target)
			return nil
		})); err != nil {
		return nil, err
	}
	touchCounter, err := meter.Int64Counter("glutton.memload.touched_pages",
		metric.WithDescription("Pages dirtied by the memload sweeper."))
	if err != nil {
		return nil, err
	}
	touchCounter.Add(ctx, m.touched.Load())

	go m.sweep(ctx, interval, pattern, touchCounter)
	return m, nil
}

// sweep re-dirties the working set forever: each interval writes as many
// pages as the set holds, sequentially or at random. Writes are paced in
// batches so the load is spread across the interval instead of bursting.
func (m *memLoad) sweep(ctx context.Context, interval time.Duration, pattern string, touchCounter metric.Int64Counter) {
	totalPages := 0
	for _, c := range m.chunks {
		totalPages += (len(c) + pageBytes - 1) / pageBytes
	}
	const batches = 64
	tick := time.NewTicker(interval / batches)
	defer tick.Stop()
	rng := rand.New(rand.NewSource(1)) // deterministic across runs for comparable benchmarks

	for {
		sweepNo := m.sweeps.Add(1)
		val := byte(sweepNo) // vary the written value so every pass really writes
		pagesPerBatch := (totalPages + batches - 1) / batches
		for b := 0; b < batches; b++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			before := m.touched.Load()
			if pattern == "sequential" {
				m.writeRange(b*pagesPerBatch, pagesPerBatch, val)
			} else {
				m.writeRandom(rng, pagesPerBatch, val)
			}
			touchCounter.Add(ctx, m.touched.Load()-before)
		}
	}
}

// writePass writes val to one byte of every page across all chunks.
func (m *memLoad) writePass(val byte, _ *rand.Rand) {
	for _, c := range m.chunks {
		for off := 0; off < len(c); off += pageBytes {
			c[off] = val
			m.touched.Add(1)
		}
	}
}

// writeRange writes val to count pages starting at global page index start.
func (m *memLoad) writeRange(start, count int, val byte) {
	idx := 0
	for _, c := range m.chunks {
		pages := (len(c) + pageBytes - 1) / pageBytes
		for p := 0; p < pages; p++ {
			if idx >= start && idx < start+count {
				c[p*pageBytes] = val
				m.touched.Add(1)
			}
			idx++
		}
	}
}

// writeRandom writes val to count uniformly random pages.
func (m *memLoad) writeRandom(rng *rand.Rand, count int, val byte) {
	for i := 0; i < count; i++ {
		c := m.chunks[rng.Intn(len(m.chunks))]
		pages := len(c) / pageBytes
		if pages == 0 {
			continue
		}
		c[rng.Intn(pages)*pageBytes] = val
		m.touched.Add(1)
	}
}

// parseBytes parses a human-friendly size: plain bytes, or a Ki/Mi/Gi
// (binary) or K/M/G (decimal) suffix, e.g. "512Mi", "2Gi", "1000000".
func parseBytes(s string) (int64, error) {
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30},
		{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000},
	}
	for _, m := range multipliers {
		if v, ok := strings.CutSuffix(s, m.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing size %q: %w", s, err)
			}
			return n * m.mult, nil
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing size %q: %w", s, err)
	}
	return n, nil
}

// logMemLoadStart is split out of main for a single, greppable startup line.
func logMemLoadStart(ctx context.Context, target int64, interval time.Duration, pattern string) {
	slog.InfoContext(ctx, "memload started",
		slog.Int64("target_bytes", target),
		slog.Duration("touch_interval", interval),
		slog.String("pattern", pattern),
	)
}
