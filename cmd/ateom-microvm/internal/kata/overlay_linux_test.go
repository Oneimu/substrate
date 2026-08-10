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

package kata

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The kernel requires overlay upperdir and workdir on the same filesystem and
// rejects a workdir nested inside (or equal to) upperdir — so they must be
// SIBLINGS under the one upper base on the ateUpper share. A layout change
// here breaks every overlay mount.
func TestUpperWorkDirsAreSiblings(t *testing.T) {
	base := UpperBase("app")
	upper, work := upperWorkDirs(base)
	if filepath.Dir(upper) != base || filepath.Dir(work) != base {
		t.Errorf("upperWorkDirs(%q) = %q, %q; want both directly under the base", base, upper, work)
	}
	if upper == work {
		t.Errorf("upperWorkDirs(%q): upper and work are the same directory %q", base, upper)
	}
	if strings.HasPrefix(work+"/", upper+"/") {
		t.Errorf("upperWorkDirs(%q): work %q is nested inside upper %q", base, work, upper)
	}
}

func TestVirtiofsdArgs(t *testing.T) {
	tests := []struct {
		name      string
		opts      VirtiofsdOptions
		wantCache string
		wantXattr bool
	}{
		{
			name:      "RO lower defaults to cache=always",
			opts:      VirtiofsdOptions{SocketPath: "/run/vm/virtiofsd.sock", SharedDir: "/run/shared"},
			wantCache: "--cache=always",
		},
		{
			name: "writable durable share overrides the cache mode",
			opts: VirtiofsdOptions{
				SocketPath: "/run/vm/virtiofsd-durable.sock",
				SharedDir:  "/var/lib/ateom-gvisor/actors/uid/durable-dir",
				Cache:      "auto",
			},
			wantCache: "--cache=auto",
		},
		{
			name: "rootfs upper share passes xattrs through",
			opts: VirtiofsdOptions{
				SocketPath: "/run/vm/virtiofsd-upper.sock",
				SharedDir:  "/var/lib/ateom-gvisor/actors/uid/rootfs-upper",
				Cache:      "auto",
				Xattr:      true,
			},
			wantCache: "--cache=auto",
			wantXattr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := virtiofsdArgs(tc.opts)
			if !slices.Contains(args, tc.wantCache) {
				t.Errorf("args %v do not contain %q", args, tc.wantCache)
			}
			// Overlay whiteouts/opaque markers are user.overlay.* xattrs in the
			// upper; a share hosting an upper must pass them through, and the
			// others must not pay the passthrough cost.
			if gotXattr := slices.Contains(args, "--xattr"); gotXattr != tc.wantXattr {
				t.Errorf("args %v: --xattr present = %v, want %v", args, gotXattr, tc.wantXattr)
			}
			for _, want := range []string{
				"--socket-path=" + tc.opts.SocketPath,
				"--shared-dir=" + tc.opts.SharedDir,
				// find-paths re-opens the guest's open files by path on
				// restore; both shares depend on it.
				"--migration-mode",
				"find-paths",
			} {
				if !slices.Contains(args, want) {
					t.Errorf("args %v do not contain %q", args, want)
				}
			}
		})
	}
}
