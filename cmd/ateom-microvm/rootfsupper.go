//go:build linux

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

// Disk-backed rootfs writes for the micro-VM runtime.
//
// Every container's overlay upper lives on a THIRD virtio-fs share
// (kata.UpperFsTag), served by its own virtiofsd from
// ateompath.RootfsUpperDir(actorUID) on the host: rootfs writes cost host disk,
// not guest RAM. (The retired alternative — a guest tmpfs upper — capped rootfs
// writes at the tmpfs size, a fifth of guest RAM, and pinned every written byte
// in memory; snapshots taken in that mode still restore, see below.)
//
// The share is served like the durable-dir one — write-through (no
// --writeback, so a paused guest's completed writes are already on the host),
// cache=auto (the host contents change underneath the guest on restore), and
// find-paths migration — plus --xattr, because overlayfs stores whiteouts and
// opaque-directory markers as user.overlay.* xattrs in the upper and the
// guest kernel must round-trip them through virtiofsd. Unlike the durable dirs
// (whose host side atelet owns), this directory is owned entirely by ateom:
// created fresh at cold boot, re-materialized from the snapshot at restore,
// and removed at teardown.
//
// Snapshots: the upper does not ride in guest memory, so a FULL snapshot
// ships it as a tar (rootfsUpperTarFile) exactly like the durable volumes,
// taken while the guest is paused. Restore is self-describing — the tar's
// presence in the snapshot is what says the guest expects the ateUpper share
// (the snapshot's config.json references its fs device) — which is also what
// keeps legacy tmpfs-upper snapshots restorable: no tar, no share, their upper
// rides inside the restored guest memory. A DATA snapshot deliberately
// excludes rootfs state: the workload cold-starts on restore.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/tarutil"
	"github.com/agent-substrate/substrate/internal/ateompath"
)

// rootfsUpperTarFile is the snapshot file holding the tar of the actor's
// rootfs uppers. Its entries are <containerID>/fs/... and <containerID>/work/...
// relative to ateompath.RootfsUpperDir, so extraction restores the exact layout
// the guest's find-paths re-opens.
const rootfsUpperTarFile = "rootfs-upper.tar"

// upperVirtiofsdLogPath is where the rootfs upper share's virtiofsd logs,
// beside the overlay lower's and the durable share's under the actor's VM dir.
func upperVirtiofsdLogPath(id string) string {
	return filepath.Join(kata.VMDir(id), "virtiofsd-upper.log")
}

// resetRootfsUpperDir gives a cold boot a pristine upper directory: a cold
// boot must start from the bare image, and atelet's actor-dir reset does not
// know about this directory, so ateom wipes any previous activation's contents
// itself.
func resetRootfsUpperDir(actorUID string) error {
	dir := ateompath.RootfsUpperDir(actorUID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("while clearing rootfs upper dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("while creating rootfs upper dir %q: %w", dir, err)
	}
	return nil
}

// actorHasDiskUpper reports whether the running actor's rootfs uppers are
// disk-backed, by the host directory only a disk-upper boot/restore creates
// (and teardownActor removes). A LEGACY actor — restored from a snapshot taken
// by the retired tmpfs-upper implementation — has no directory: its upper
// lives inside guest memory, and its checkpoints must keep capturing it there.
func actorHasDiskUpper(actorUID string) bool {
	_, err := os.Stat(ateompath.RootfsUpperDir(actorUID))
	return err == nil
}

// snapshotHasRootfsUpper reports whether a snapshot carries disk-backed rootfs
// uppers — i.e. whether its guest expects the ateUpper share on restore.
func snapshotHasRootfsUpper(snapshotDir string) bool {
	_, err := os.Stat(filepath.Join(snapshotDir, rootfsUpperTarFile))
	return err == nil
}

// stageRootfsUpperShare starts the virtiofsd serving the actor's rootfs
// uppers. The caller has already created (cold boot) or re-materialized
// (restore) the host directory.
//
// The returned cmd outlives this call (CH talks to it for the VM's lifetime);
// the caller owns it (tracked on runningActor, killed in teardownActor).
func (s *AteomService) stageRootfsUpperShare(ctx context.Context, rr resolvedRuntime, actorUID string) (*exec.Cmd, error) {
	shared := ateompath.RootfsUpperDir(actorUID)
	if _, err := os.Stat(shared); err != nil {
		return nil, fmt.Errorf("while checking rootfs upper dir %q: %w", shared, err)
	}
	log, _ := os.OpenFile(upperVirtiofsdLogPath(actorUID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	cmd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary:     rr.virtiofsd,
		SocketPath: kata.UpperVirtiofsdSocketPath(actorUID),
		SharedDir:  shared,
		Cache:      "auto",
		Xattr:      true,
		Log:        log,
	})
	if err != nil {
		return nil, fmt.Errorf("while starting rootfs upper virtiofsd: %w", err)
	}
	return cmd, nil
}

// tarRootfsUpper archives the actor's rootfs uppers (dir) into the checkpoint
// directory. The caller must have paused the guest first: virtiofsd is
// write-through, so a completed guest write is on the host by then, but a
// running guest could still add more after the walk.
func tarRootfsUpper(ctx context.Context, dir, checkpointDir string) error {
	if err := tarutil.Create(ctx, filepath.Join(checkpointDir, rootfsUpperTarFile), dir); err != nil {
		return fmt.Errorf("while archiving rootfs uppers from %q: %w", dir, err)
	}
	return nil
}

// untarRootfsUpper restores the rootfs uppers from a snapshot into the actor's
// host directory. It must run before the upper share's virtiofsd starts, so
// the guest never observes the directory mid-restore. The directory is
// recreated from scratch: nothing else owns it, and stale contents from a
// previous activation would corrupt the overlay state find-paths re-opens.
func untarRootfsUpper(dir, snapshotDir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("while clearing rootfs upper dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("while creating rootfs upper dir %q: %w", dir, err)
	}
	if err := tarutil.Extract(filepath.Join(snapshotDir, rootfsUpperTarFile), dir); err != nil {
		return fmt.Errorf("while restoring rootfs uppers into %q: %w", dir, err)
	}
	return nil
}
