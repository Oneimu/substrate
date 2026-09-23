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

"""Parser and aggregation tests for phase_report."""

import json

import phase_report


def parse(lines):
    out = phase_report.Parsed()
    for line in lines:
        phase_report.parse_line(json.loads(line), out)
    return out


ATELET_RESTORE = json.dumps({
    "time": "2026-09-23T10:00:05Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full", "ate.snapshot.kind": "latest",
    "ate.actor.restore.duration.download": 2.4,
    "ate.actor.restore.duration.ateom_restore": 1.1,
    "ate.actor.restore.duration.total": 3.9,
})

ATEOM_RESTORE = json.dumps({
    "time": "2026-09-23T10:00:04Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full",
    "ateom.actor.restore.duration.vm_restore": 0.8,
    "ateom.actor.restore.duration.wakeup_probe": 0.2,
    "ateom.actor.restore.duration.total": 1.05,
})

ATEOM_CHECKPOINT = json.dumps({
    "time": "2026-09-23T10:01:00Z", "level": "INFO",
    "msg": "Checkpoint timing breakdown",
    "ate.actor.uid": "uid-1", "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.scope": "full",
    "ateom.actor.checkpoint.duration.pause": 0.01,
    "ateom.actor.checkpoint.duration.snapshot": 0.5,
    "ateom.actor.checkpoint.duration.merge": 0.005,
    "ateom.actor.checkpoint.duration.total": 0.6,
    "ateom.snapshot.delta.size": 52428800,
})

TRANSFER = json.dumps({
    "time": "2026-09-23T10:01:02Z", "level": "INFO",
    "msg": "Snapshot transfer breakdown",
    "ate.template.name": "swebench-astropy-7336",
    "ate.snapshot.phase": "persist", "file.name": "memory-ranges",
    "atelet.snapshot.transfer.duration": 1.8,
    "atelet.snapshot.transfer.size.logical": 1073741824,
    "atelet.snapshot.transfer.size.populated": 314572800,
    "atelet.snapshot.transfer.size.wire": 125829120,
})

FAILED = json.dumps({
    "time": "2026-09-23T10:02:00Z", "level": "INFO",
    "msg": "Restore timing breakdown",
    "ate.actor.uid": "uid-2", "ate.snapshot.scope": "full",
    "error.type": "FAILED_GET_EXTERNAL_OBJECT",
    "ate.actor.restore.duration.download": 30.0,
    "ate.actor.restore.duration.total": 30.0,
})


def test_parses_both_layers_of_the_same_msg():
    out = parse([ATELET_RESTORE, ATEOM_RESTORE])
    assert [(b.source, b.op) for b in out.breakdowns] == [
        ("atelet", "restore"), ("ateom", "restore")]
    assert out.breakdowns[0].phases["download"] == 2.4
    assert out.breakdowns[1].phases["vm_restore"] == 0.8
    assert out.breakdowns[1].actor_uid == "uid-1"


def test_checkpoint_carries_delta_size():
    out = parse([ATEOM_CHECKPOINT])
    b = out.breakdowns[0]
    assert (b.source, b.op) == ("ateom", "checkpoint")
    assert b.delta_bytes == 52428800


def test_transfer_record():
    out = parse([TRANSFER])
    t = out.transfers[0]
    assert t.phase == "persist"
    assert t.file_name == "memory-ranges"
    assert t.bytes == {
        "logical": 1073741824, "populated": 314572800, "wire": 125829120}


def test_failed_records_are_flagged_and_excluded_from_percentiles():
    out = parse([ATELET_RESTORE, FAILED])
    assert [b.failed for b in out.breakdowns] == [False, True]
    rows = phase_report.report_phases(out.breakdowns, lambda _: None)
    downloads = [r for r in rows if r["phase"] == "download"]
    assert len(downloads) == 1
    assert downloads[0]["count"] == 1  # the failed 30s never entered


def test_prefixed_kubectl_lines_still_parse(tmp_path):
    log = tmp_path / "pod.log"
    log.write_text("pod/atelet-abc " + ATELET_RESTORE + "\nnot json\n")
    out = phase_report.parse_files([str(log)])
    assert len(out.breakdowns) == 1


def test_waterfall_joins_ateom_under_atelet(capsys):
    out = parse([ATEOM_RESTORE, ATELET_RESTORE])
    phase_report.report_waterfalls(out.breakdowns, print, slowest=1)
    text = capsys.readouterr().out
    assert "atelet ateom_restore" in text
    assert "ateom  vm_restore" in text
