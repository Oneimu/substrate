#!/usr/bin/env bash
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

# Dump the node-side logs a benchmark run needs for phase_report.py: every
# atelet pod (the atelet-side timing breakdowns and transfer records) and
# every worker pod (ateom-microvm writes its records to the worker pod's
# stdout through the shared log writer). Point --since at the run's start so
# the report covers exactly one run.
#
# Usage: collect_logs.sh --dest DIR [--since 30m] [--namespace ate-system]
#                        [--worker-namespace NS]

set -euo pipefail

DEST=""
SINCE="1h"
NAMESPACE="ate-system"
WORKER_NAMESPACE="default"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dest) DEST="$2"; shift 2 ;;
    --since) SINCE="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --worker-namespace) WORKER_NAMESPACE="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "${DEST}" ]]; then
  echo "usage: $0 --dest DIR [--since 30m] [--namespace ate-system] [--worker-namespace NS]" >&2
  exit 2
fi
mkdir -p "${DEST}"

collect() {
  local ns="$1" selector="$2"
  local pods
  pods=$(kubectl get pods -n "${ns}" -l "${selector}" -o name)
  for pod in ${pods}; do
    local name="${pod#pod/}"
    echo "collecting ${ns}/${name} (since ${SINCE})"
    kubectl logs -n "${ns}" "${name}" --since="${SINCE}" --timestamps=false \
      > "${DEST}/${ns}-${name}.log"
  done
}

collect "${NAMESPACE}" "app=atelet"
# Worker pods carry the pool label whatever the pool's name is.
collect "${WORKER_NAMESPACE}" "ate.dev/worker-pool"

echo "logs in ${DEST}; next:"
echo "  python3 benchmarking/analysis/phase_report.py ${DEST}/*.log"
