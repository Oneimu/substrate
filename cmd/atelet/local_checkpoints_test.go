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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSnapshotDir(t *testing.T, dir, prefix string) {
	t.Helper()
	p := filepath.Join(dir, prefix)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "memory.img"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRemovesEverySnapshot(t *testing.T) {
	dir := t.TempDir()
	writeSnapshotDir(t, dir, "pause-1")
	writeSnapshotDir(t, dir, "pause-2")
	writeSnapshotDir(t, dir, "pause-3")

	if err := pruneLocalCheckpointDir(context.Background(), dir); err != nil {
		t.Fatalf("pruneLocalCheckpointDir() = %v, want nil", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists (err=%v), want removed entirely", err)
	}
}

func TestPruneMissingDirIsNoop(t *testing.T) {
	if err := pruneLocalCheckpointDir(context.Background(), filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("pruneLocalCheckpointDir() = %v, want nil", err)
	}
}

// An undeletable snapshot must be reported — Terminate turns that error into a
// failed RPC so the delete workflow retries — without stranding the snapshots
// that could have been removed.
func TestPruneReportsFailureAndStillRemovesTheRest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so no snapshot can be made undeletable")
	}
	dir := t.TempDir()
	writeSnapshotDir(t, dir, "pause-1")
	writeSnapshotDir(t, dir, "pause-2")

	// A snapshot dir with no write bit: its files cannot be unlinked.
	stuck := filepath.Join(dir, "pause-stuck")
	writeSnapshotDir(t, dir, "pause-stuck")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	err := pruneLocalCheckpointDir(context.Background(), dir)
	if err == nil {
		t.Fatal("pruneLocalCheckpointDir() = nil, want an error naming the undeletable snapshot")
	}
	if !strings.Contains(err.Error(), "pause-stuck") {
		t.Errorf("pruneLocalCheckpointDir() = %v, want it to name pause-stuck", err)
	}
	for _, name := range []string{"pause-1", "pause-2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v), want removed", name, err)
		}
	}
}

func TestPruneKeepsTheNamedSnapshot(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pause-1", "pause-2"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "checkpoint.img"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := planLocalCheckpointPruneDir(dir, "pause-2")
	if err != nil {
		t.Fatalf("planLocalCheckpointPruneDir() = %v, want nil", err)
	}
	if want := []string{filepath.Join(dir, "pause-1")}; !slices.Equal(plan.paths, want) {
		t.Fatalf("planned paths = %v, want %v", plan.paths, want)
	}
	// A snapshot written after the plan is not the plan's to remove.
	if err := os.MkdirAll(filepath.Join(dir, "pause-3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := plan.run(context.Background()); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pause-1")); !os.IsNotExist(err) {
		t.Errorf("pause-1 survived the prune (stat err = %v)", err)
	}
	for _, name := range []string{"pause-2", "pause-3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s did not survive the prune: %v", name, err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the local checkpoints dir was removed although a snapshot was kept: %v", err)
	}
}

func TestPruneKeepOfMissingDirPlansNothing(t *testing.T) {
	plan, err := planLocalCheckpointPruneDir(filepath.Join(t.TempDir(), "absent"), "pause-1")
	if err != nil {
		t.Fatalf("planLocalCheckpointPruneDir() = %v, want nil", err)
	}
	if len(plan.paths) != 0 {
		t.Fatalf("planned paths = %v, want none", plan.paths)
	}
	if err := plan.run(context.Background()); err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
}
