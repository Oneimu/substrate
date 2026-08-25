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
)

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "512Mi", want: 512 << 20},
		{in: "2Gi", want: 2 << 30},
		{in: "16Ki", want: 16 << 10},
		{in: "1G", want: 1_000_000_000},
		{in: "4096", want: 4096},
		{in: "", wantErr: true},
		{in: "Gi", wantErr: true},
		{in: "1.5Gi", wantErr: true},
		{in: "-1Gi", want: -(1 << 30)}, // rejected later by startMemLoad
	}
	for _, tc := range cases {
		got, err := parseBytes(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseBytes(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStartMemLoadAllocatesAndDirtiesTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const target = 4 << 20 // 4MiB keeps the test fast; spans multiple batches
	m, err := startMemLoad(ctx, target, 100*time.Millisecond, "sequential")
	if err != nil {
		t.Fatalf("startMemLoad: %v", err)
	}

	var total int64
	for _, c := range m.chunks {
		total += int64(len(c))
	}
	if total != target {
		t.Errorf("allocated %d bytes, want %d", total, target)
	}

	// startMemLoad returns only after the initial full pass, so every page
	// must already be dirtied.
	wantPages := int64(target / pageBytes)
	if got := m.touched.Load(); got < wantPages {
		t.Errorf("touched %d pages after initial pass, want >= %d", got, wantPages)
	}
	// Spot-check that the initial pass really wrote.
	if m.chunks[0][0] != 1 {
		t.Errorf("chunks[0][0] = %d, want 1 (initial pass value)", m.chunks[0][0])
	}
}

func TestMemLoadSweeperKeepsTouching(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, err := startMemLoad(ctx, 1<<20, 50*time.Millisecond, "random")
	if err != nil {
		t.Fatalf("startMemLoad: %v", err)
	}
	after := m.touched.Load()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.touched.Load() > after {
			return // sweeper is running
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sweeper made no progress: touched stuck at %d", after)
}

func TestStartMemLoadRejectsBadInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := startMemLoad(ctx, 0, time.Second, "sequential"); err == nil {
		t.Error("startMemLoad(0 bytes) succeeded, want error")
	}
	if _, err := startMemLoad(ctx, 1<<20, -time.Second, "sequential"); err == nil {
		t.Error("startMemLoad(negative interval) succeeded, want error")
	}
	if _, err := startMemLoad(ctx, 1<<20, 0, "sequential"); err == nil {
		t.Error("startMemLoad(zero interval) succeeded, want error")
	}
	if _, err := startMemLoad(ctx, 1<<20, time.Second, "zigzag"); err == nil {
		t.Error("startMemLoad(pattern=zigzag) succeeded, want error")
	}
}
