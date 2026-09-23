# Suspend/resume phase analysis

This directory turns the node side's developer-facing timing logs into the
percentiles a benchmark run needs, without making the phases metric API
(phases are implementation details; metrics are a contract — the histograms
stop at `ateom_restore` / `ateom_checkpoint` / `persist` on purpose).

Three joinable JSON log records feed it:

| record (`msg`) | emitter | keys |
|---|---|---|
| `Restore timing breakdown` | atelet and ateom-microvm | `ate.actor.restore.duration.<phase>` / `ateom.actor.restore.duration.<phase>` |
| `Checkpoint timing breakdown` | atelet and ateom-microvm | `ate.actor.checkpoint.duration.<phase>` / `ateom.actor.checkpoint.duration.<phase>`, plus `ateom.snapshot.delta.size` after an OnDemand merge |
| `Snapshot transfer breakdown` | atelet, one per file per transfer | `atelet.snapshot.transfer.duration`, `atelet.snapshot.transfer.size.{logical,populated,wire}` |

Every record carries the full actor identity (`ate.actor.uid`, template,
scope), which the histograms are barred from, so records join per actor and
per operation.

## Making a measurement

1. Run a suspend/resume-heavy load (e.g. the `sweperf` or `glutton` user
   class; see `../README.md`).
2. Dump the node logs for the run's window:

   ```bash
   ./collect_logs.sh --dest /tmp/run1 --since 30m
   ```

3. Aggregate:

   ```bash
   python3 phase_report.py /tmp/run1/*.log --csv /tmp/run1/report
   ```

The report prints per-phase percentiles at both layers (atelet's
volume_mount → download → oci_unpack → ateom_* → persist, and inside that
ateom's pause/snapshot/merge/teardown for a checkpoint or
prep/…/vm_restore/wakeup_probe for a restore), per-file transfer time, bytes
(logical vs populated vs wire) and throughput, and the slowest operations as
nested waterfalls.

Reading it for a slow **SuspendActor**: the atelet `checkpoint` rows split
the time between `sandbox_assets`, `ateom_checkpoint` and `persist`; the
ateom `checkpoint` rows split `ateom_checkpoint` further (the paused window
costs max(snapshot, durable_dir, rootfs_upper), then merge and teardown); the
`persist`-phase transfer rows say which file the upload spent it on and at
what throughput; and `ateom.snapshot.delta.size` against the memory image's
populated bytes says what a differential upload would save.

The parser is deliberately tolerant: it scans any line for a JSON object and
matches on `msg`, so raw `kubectl logs` dumps (even with prefixes) work.
