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

"""Parser and aggregation tests for phase_report. No cluster needed:

    python3 -m unittest discover -s benchmarking/analysis
"""

import io
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import phase_report  # noqa: E402


def parse(*records):
    out = phase_report.Parsed()
    for rec in records:
        phase_report.parse_line(json.loads(json.dumps(rec)), out)
    return out


def render(fn, *args, **kwargs):
    buf = io.StringIO()
    fn(*args, writer=lambda line: buf.write(line + "\n"), **kwargs)
    return buf.getvalue()


ATELET_RESTORE = {
    "time": "2026-09-23T10:00:05.500000000Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full", "ate.snapshot.kind": "latest",
    "ate.actor.restore.duration.download": 2.4,
    "ate.actor.restore.duration.ateom_restore": 1.1,
    "ate.actor.restore.duration.total": 3.9,
}

ATEOM_RESTORE = {
    "time": "2026-09-23T10:00:05.400000000Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full",
    "ateom.actor.restore.duration.vm_restore": 0.8,
    "ateom.actor.restore.duration.wakeup_probe": 0.2,
    "ateom.actor.restore.duration.total": 1.05,
}

ATELET_CHECKPOINT = {
    "time": "2026-09-23T10:01:00.000000000Z", "level": "INFO",
    "msg": "Checkpoint timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full", "ate.snapshot.kind": "latest",
    "ate.actor.checkpoint.duration.sandbox_assets": 0.01,
    "ate.actor.checkpoint.duration.ateom_checkpoint": 1.14,
    "ate.actor.checkpoint.duration.persist": 4.48,
    "ate.actor.checkpoint.duration.total": 6.0,
}

ATEOM_CHECKPOINT = {
    "time": "2026-09-23T10:00:55.000000000Z", "level": "INFO",
    "msg": "Checkpoint timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full",
    "ateom.actor.checkpoint.duration.prep": 0.04,
    "ateom.actor.checkpoint.duration.pause": 0.01,
    "ateom.actor.checkpoint.duration.snapshot": 0.5,
    "ateom.actor.checkpoint.duration.rootfs_upper": 0.9,
    "ateom.actor.checkpoint.duration.teardown": 0.2,
    "ateom.actor.checkpoint.duration.total": 1.2,
}

FAILED = {
    "time": "2026-09-23T10:02:00Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-2", "ate.snapshot.scope": "full",
    "error.type": "DeadlineExceeded",
    "ate.actor.restore.duration.download": 30.0,
    "ate.actor.restore.duration.total": 30.0,
}


class ParseTest(unittest.TestCase):
    def test_parses_both_layers_of_the_same_msg(self):
        out = parse(ATELET_RESTORE, ATEOM_RESTORE)
        self.assertEqual([(b.source, b.op) for b in out.breakdowns],
                         [("atelet", "restore"), ("ateom", "restore")])
        self.assertEqual(out.breakdowns[0].phases["download"], 2.4)
        self.assertEqual(out.breakdowns[1].phases["vm_restore"], 0.8)
        self.assertEqual(out.breakdowns[1].actor_uid, "uid-1")

    def test_nanosecond_timestamps_parse(self):
        out = parse(ATELET_RESTORE, FAILED)
        self.assertIsNotNone(out.breakdowns[0].ts)
        self.assertIsNotNone(out.breakdowns[1].ts)
        self.assertAlmostEqual(out.breakdowns[0].ts % 1, 0.5, places=6)
        self.assertIsNone(phase_report.parse_time("yesterday"))

    def test_ateom_checkpoint_parses(self):
        b = parse(ATEOM_CHECKPOINT).breakdowns[0]
        self.assertEqual((b.source, b.op), ("ateom", "checkpoint"))
        self.assertEqual(b.phases["rootfs_upper"], 0.9)

    def test_prefixed_kubectl_lines_still_parse(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "pod.log")
            with open(path, "w") as f:
                f.write("pod/atelet-abc " + json.dumps(ATELET_RESTORE) + "\nnot json\n{\"msg\": \"other\"}\n")
            out = phase_report.parse_files([path])
        self.assertEqual(len(out.breakdowns), 1)
        self.assertEqual(out.lines_seen, 2)


class UnattributedTest(unittest.TestCase):
    def test_atelet_checkpoint_residual_is_total_minus_sequential_phases(self):
        b = parse(ATELET_CHECKPOINT).breakdowns[0]
        self.assertAlmostEqual(b.phases["unattributed"], 6.0 - (0.01 + 1.14 + 4.48))

    def test_ateom_checkpoint_counts_the_concurrent_captures_once(self):
        b = parse(ATEOM_CHECKPOINT).breakdowns[0]
        # prep + pause + max(snapshot, rootfs_upper) + teardown; the two
        # captures overlapped, so only the slower one is spent wall time.
        self.assertAlmostEqual(b.phases["unattributed"], 1.2 - (0.04 + 0.01 + 0.9 + 0.2))

    def test_restore_layers_get_no_residual(self):
        out = parse(ATELET_RESTORE, ATEOM_RESTORE)
        for b in out.breakdowns:
            self.assertNotIn("unattributed", b.phases)

    def test_residual_never_negative(self):
        rec = dict(ATELET_CHECKPOINT, **{"ate.actor.checkpoint.duration.total": 1.0})
        self.assertEqual(parse(rec).breakdowns[0].phases["unattributed"], 0.0)


class ReportTest(unittest.TestCase):
    def test_failed_records_are_flagged_and_excluded_from_percentiles(self):
        out = parse(ATELET_RESTORE, FAILED)
        self.assertEqual([b.failed for b in out.breakdowns], [False, True])
        rows = phase_report.report_phases(out.breakdowns, lambda _: None)
        downloads = [r for r in rows if r["phase"] == "download"]
        self.assertEqual(len(downloads), 1)
        self.assertEqual(downloads[0]["count"], 1)  # the failed 30s never entered

    def test_waterfall_nests_ateom_and_gap_under_atelet(self):
        out = parse(ATEOM_RESTORE, ATELET_RESTORE)
        text = render(phase_report.report_waterfalls, out.breakdowns, slowest=1)
        lines = [line.strip() for line in text.splitlines()]
        self.assertTrue(any(line.startswith("atelet ateom_restore") for line in lines), text)
        self.assertTrue(any(line.startswith("ateom  vm_restore") for line in lines), text)
        gap = [line for line in lines if line.startswith("(gap)")]
        self.assertEqual(len(gap), 1, text)
        self.assertIn("50.0", gap[0])  # 1.1s atelet ateom_restore - 1.05s ateom total


if __name__ == "__main__":
    unittest.main()
