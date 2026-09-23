#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Aggregate the suspend/resume phase-breakdown log records into a report.

Reads JSON-lines logs (kubectl logs dumps of the worker pods; any non-JSON or
unrelated lines are skipped) and aggregates the three joinable record kinds
the node side emits:

  - "Restore timing breakdown"     atelet (ate.actor.restore.duration.*)
                                   and ateom (ateom.actor.restore.duration.*)
  - "Checkpoint timing breakdown"  atelet (ate.actor.checkpoint.duration.*)
                                   and ateom (ateom.actor.checkpoint.duration.*)
  - "Snapshot transfer breakdown"  atelet, one record per file per transfer
                                   (atelet.snapshot.transfer.*)

The report answers "where does the SuspendActor / ResumeActor time go":
per-phase percentiles at each layer, per-file transfer time, bytes and
throughput, and the slowest operations as nested waterfalls (the ateom record
joined under the atelet record of the same actor by time adjacency).

Usage:
    phase_report.py run-logs/*.log [--csv DEST_DIR] [--slowest N]

The records are developer-facing logs, not metric API — this reader is the
consumer that makes them percentiles. See benchmarking/analysis/README.md.
"""

from __future__ import annotations

import argparse
import csv
import json
import statistics
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path

# The duration-key prefixes, source of truth: cmd/atelet/metrics.go
# (restoreDurationMetric, checkpointDurationMetric), cmd/atelet/transferlog.go
# and cmd/ateom-microvm/phaselog.go.
BREAKDOWN_PREFIXES = {
    ("atelet", "restore"): "ate.actor.restore.duration.",
    ("atelet", "checkpoint"): "ate.actor.checkpoint.duration.",
    ("ateom", "restore"): "ateom.actor.restore.duration.",
    ("ateom", "checkpoint"): "ateom.actor.checkpoint.duration.",
}
TRANSFER_DURATION_KEY = "atelet.snapshot.transfer.duration"
TRANSFER_SIZE_PREFIX = "atelet.snapshot.transfer.size."
DELTA_SIZE_KEY = "ateom.snapshot.delta.size"

BREAKDOWN_MSGS = {"Restore timing breakdown", "Checkpoint timing breakdown"}
TRANSFER_MSG = "Snapshot transfer breakdown"

ACTOR_UID_KEY = "ate.actor.uid"
SCOPE_KEY = "ate.snapshot.scope"
PHASE_KEY = "ate.snapshot.phase"
KIND_KEY = "ate.snapshot.kind"
TEMPLATE_KEY = "ate.template.name"
ERROR_TYPE_KEY = "error.type"
FILE_NAME_KEY = "file.name"

# Sequential order of the known phases, for waterfall display. Phases absent
# from a record simply don't print; unknown phases print after, input order.
PHASE_ORDER = [
    # atelet restore
    "volume_mount", "manifest_fetch", "sandbox_assets", "download",
    "oci_unpack", "ateom_restore",
    # ateom restore
    "prep", "bundles", "upper_join", "lowers", "tap", "vmm_launch",
    "vm_restore", "resume", "wakeup_probe",
    # atelet + ateom checkpoint
    "pause", "snapshot", "durable_dir", "rootfs_upper", "merge",
    "ateom_checkpoint", "teardown", "persist",
    "total",
]
_PHASE_RANK = {name: i for i, name in enumerate(PHASE_ORDER)}


@dataclass
class Breakdown:
    """One parsed timing-breakdown record."""
    source: str  # atelet | ateom
    op: str  # restore | checkpoint
    time: str
    actor_uid: str
    template: str
    scope: str
    kind: str
    phases: dict[str, float]  # phase name -> seconds
    failed: bool
    delta_bytes: int | None = None


@dataclass
class Transfer:
    """One parsed per-file transfer record."""
    time: str
    template: str
    phase: str  # persist | download
    file_name: str
    duration_s: float
    bytes: dict[str, int]  # logical | populated | wire (present kinds only)


@dataclass
class Parsed:
    breakdowns: list[Breakdown] = field(default_factory=list)
    transfers: list[Transfer] = field(default_factory=list)
    lines_seen: int = 0
    lines_matched: int = 0


def parse_line(obj: dict, out: Parsed) -> None:
    msg = obj.get("msg", "")
    if msg in BREAKDOWN_MSGS:
        for (source, op), prefix in BREAKDOWN_PREFIXES.items():
            phases = {
                k[len(prefix):]: float(v)
                for k, v in obj.items()
                if k.startswith(prefix)
            }
            if not phases:
                continue
            delta = obj.get(DELTA_SIZE_KEY)
            out.breakdowns.append(Breakdown(
                source=source,
                op=op,
                time=obj.get("time", ""),
                actor_uid=obj.get(ACTOR_UID_KEY, ""),
                template=obj.get(TEMPLATE_KEY, ""),
                scope=obj.get(SCOPE_KEY, ""),
                kind=obj.get(KIND_KEY, ""),
                phases=phases,
                failed=ERROR_TYPE_KEY in obj,
                delta_bytes=int(delta) if delta is not None else None,
            ))
            out.lines_matched += 1
            return
    elif msg == TRANSFER_MSG and TRANSFER_DURATION_KEY in obj:
        out.transfers.append(Transfer(
            time=obj.get("time", ""),
            template=obj.get(TEMPLATE_KEY, ""),
            phase=obj.get(PHASE_KEY, ""),
            file_name=obj.get(FILE_NAME_KEY, ""),
            duration_s=float(obj[TRANSFER_DURATION_KEY]),
            bytes={
                k[len(TRANSFER_SIZE_PREFIX):]: int(v)
                for k, v in obj.items()
                if k.startswith(TRANSFER_SIZE_PREFIX)
            },
        ))
        out.lines_matched += 1


def parse_files(paths: list[str]) -> Parsed:
    out = Parsed()
    for path in paths:
        f = sys.stdin if path == "-" else open(path, encoding="utf-8", errors="replace")
        with f:
            for line in f:
                # kubectl log dumps may prefix each line (pod name, timestamp);
                # recover the JSON object from the first brace.
                brace = line.find("{")
                if brace < 0:
                    continue
                out.lines_seen += 1
                try:
                    obj = json.loads(line[brace:])
                except json.JSONDecodeError:
                    continue
                if isinstance(obj, dict):
                    parse_line(obj, out)
    return out


def percentile(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[int(q) - 1]


def phase_sort_key(name: str) -> tuple[int, str]:
    return (_PHASE_RANK.get(name, len(PHASE_ORDER)), name)


def fmt_s(seconds: float) -> str:
    return f"{seconds * 1000:8.1f}"


def fmt_bytes(n: float) -> str:
    for unit in ("B", "KiB", "MiB", "GiB"):
        if abs(n) < 1024 or unit == "GiB":
            return f"{n:7.1f} {unit}"
        n /= 1024
    return f"{n:.1f}"


def report_phases(breakdowns: list[Breakdown], writer) -> list[dict]:
    """Per (source, op, scope, phase) percentiles. Returns the rows for CSV."""
    groups: dict[tuple, list[float]] = defaultdict(list)
    for b in breakdowns:
        if b.failed:
            continue
        for name, seconds in b.phases.items():
            groups[(b.source, b.op, b.scope or "-", name)].append(seconds)

    rows = []
    writer("== Phase percentiles (ms) ==")
    writer(f"{'layer':7} {'op':11} {'scope':15} {'phase':15} {'n':>5} "
           f"{'p50':>8} {'p90':>8} {'p95':>8} {'max':>8}")
    for key in sorted(groups, key=lambda k: (k[0], k[1], k[2], phase_sort_key(k[3]))):
        vals = sorted(groups[key])
        source, op, scope, name = key
        row = {
            "layer": source, "op": op, "scope": scope, "phase": name,
            "count": len(vals),
            "p50_ms": percentile(vals, 50) * 1000,
            "p90_ms": percentile(vals, 90) * 1000,
            "p95_ms": percentile(vals, 95) * 1000,
            "max_ms": max(vals) * 1000,
        }
        rows.append(row)
        writer(f"{source:7} {op:11} {scope:15} {name:15} {len(vals):5d} "
               f"{fmt_s(percentile(vals, 50))} {fmt_s(percentile(vals, 90))} "
               f"{fmt_s(percentile(vals, 95))} {fmt_s(max(vals))}")
    failed = sum(1 for b in breakdowns if b.failed)
    if failed:
        writer(f"(excluded {failed} failed operation records)")
    return rows


def report_transfers(transfers: list[Transfer], writer) -> list[dict]:
    """Per (phase, file) durations, sizes and throughput. Returns CSV rows."""
    groups: dict[tuple, list[Transfer]] = defaultdict(list)
    for t in transfers:
        groups[(t.phase, t.file_name)].append(t)

    rows = []
    writer("")
    writer("== Snapshot transfers, per file ==")
    writer(f"{'phase':9} {'file':22} {'n':>5} {'p50 ms':>8} {'p95 ms':>8} "
           f"{'wire p50':>11} {'popul p50':>11} {'zstd save':>9} {'MB/s p50':>8}")
    for key in sorted(groups):
        ts = groups[key]
        durs = sorted(t.duration_s for t in ts)
        wires = sorted(t.bytes["wire"] for t in ts if "wire" in t.bytes)
        pops = sorted(t.bytes["populated"] for t in ts if "populated" in t.bytes)
        wire_p50 = percentile(wires, 50) if wires else 0.0
        pop_p50 = percentile(pops, 50) if pops else 0.0
        dur_p50 = percentile(durs, 50)
        # Compression saving: populated bytes that did not cross the network.
        saving = 1 - wire_p50 / pop_p50 if pop_p50 else 0.0
        mbps = wire_p50 / dur_p50 / 1e6 if dur_p50 and wire_p50 else 0.0
        row = {
            "phase": key[0], "file": key[1], "count": len(ts),
            "p50_ms": dur_p50 * 1000, "p95_ms": percentile(durs, 95) * 1000,
            "wire_p50_bytes": wire_p50, "populated_p50_bytes": pop_p50,
            "zstd_saving": saving, "wire_mbps_p50": mbps,
        }
        rows.append(row)
        writer(f"{key[0]:9} {key[1]:22} {len(ts):5d} {fmt_s(dur_p50)} "
               f"{fmt_s(percentile(durs, 95))} {fmt_bytes(wire_p50):>11} "
               f"{fmt_bytes(pop_p50):>11} {saving:8.0%} {mbps:8.1f}")
    return rows


def report_waterfalls(breakdowns: list[Breakdown], writer, slowest: int) -> None:
    """The slowest operations, the ateom record nested under atelet's."""
    # Join: for each atelet record, the closest earlier-or-equal ateom record
    # of the same actor and op (one cycle emits exactly one of each; the ateom
    # one always lands first, inside the atelet ateom_* phase).
    by_actor: dict[tuple, list[Breakdown]] = defaultdict(list)
    for b in breakdowns:
        if b.source == "ateom":
            by_actor[(b.actor_uid, b.op)].append(b)
    for v in by_actor.values():
        v.sort(key=lambda b: b.time)

    atelet = [b for b in breakdowns if b.source == "atelet" and not b.failed]
    atelet.sort(key=lambda b: b.phases.get("total", 0), reverse=True)

    writer("")
    writer(f"== Slowest {slowest} operations (waterfall, ms) ==")
    for b in atelet[:slowest]:
        inner = None
        candidates = [a for a in by_actor.get((b.actor_uid, b.op), [])
                      if a.time <= b.time]
        if candidates:
            inner = candidates[-1]
        total = b.phases.get("total", 0)
        writer(f"{b.op} actor={b.actor_uid} template={b.template} "
               f"scope={b.scope or '-'} kind={b.kind or '-'} "
               f"total={total * 1000:.1f}")
        inner_phase = "ateom_restore" if b.op == "restore" else "ateom_checkpoint"
        for name in sorted(b.phases, key=phase_sort_key):
            if name == "total":
                continue
            writer(f"  atelet {name:15} {fmt_s(b.phases[name])}")
            if name == inner_phase and inner:
                for iname in sorted(inner.phases, key=phase_sort_key):
                    if iname == "total":
                        continue
                    writer(f"    ateom  {iname:13} {fmt_s(inner.phases[iname])}")
                if inner.delta_bytes is not None:
                    writer(f"    ateom  {'delta size':13} {fmt_bytes(inner.delta_bytes):>9}")


def write_csv(dest: Path, name: str, rows: list[dict]) -> None:
    if not rows:
        return
    dest.mkdir(parents=True, exist_ok=True)
    path = dest / name
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
        w.writeheader()
        w.writerows(rows)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("logs", nargs="+", help="JSON-lines log files ('-' for stdin)")
    ap.add_argument("--csv", type=Path, default=None,
                    help="also write phase_percentiles.csv and transfers.csv here")
    ap.add_argument("--slowest", type=int, default=3,
                    help="number of slowest operations to print as waterfalls")
    args = ap.parse_args()

    parsed = parse_files(args.logs)
    print(f"parsed {parsed.lines_matched} phase/transfer records "
          f"out of {parsed.lines_seen} JSON log lines")
    if not parsed.breakdowns and not parsed.transfers:
        print("no matching records; are these the worker pod logs?", file=sys.stderr)
        return 1

    print()
    phase_rows = report_phases(parsed.breakdowns, print)
    transfer_rows = report_transfers(parsed.transfers, print)
    report_waterfalls(parsed.breakdowns, print, args.slowest)

    if args.csv:
        write_csv(args.csv, "phase_percentiles.csv", phase_rows)
        write_csv(args.csv, "transfers.csv", transfer_rows)
        print(f"\nCSV written to {args.csv}/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
