#!/usr/bin/env bash
# SIGTERM variant: graceful shutdown. The controller forwards SIGTERM to the
# worker, the worker checkpoints and exits, the controller exits cleanly,
# and a fresh boot resumes the job to completion.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUNTIME="${ROOT}/bench/runtime-sigterm"
CONTROLLER_BIN="${ROOT}/bin/controller"
WORKER_IMAGE="${WORKER_IMAGE:-jobctl/worker:dev}"
LIMIT="${CHAOS_LIMIT:-1000000}"
CHECKPOINT_EVERY="${CHAOS_CHECKPOINT_EVERY_PRIMES:-5000}"
SLEEP_PER_CHECKPOINT_MS="${CHAOS_SLEEP_PER_CHECKPOINT_MS:-50}"
PORT="${PORT:-8090}"

mkdir -p "${RUNTIME}"
rm -rf "${RUNTIME:?}/"*

DB="${RUNTIME}/jobs.db"
STATE_DIR="${RUNTIME}/state"
LOG="${RUNTIME}/controller.log"
LOG2="${RUNTIME}/controller.restart.log"
RESULT="${RUNTIME}/chaos-sigterm-result.json"
mkdir -p "${STATE_DIR}"

cleanup() {
  set +e
  for p in "${CONTROLLER_PID:-}" "${CONTROLLER_PID2:-}"; do
    [[ -n "${p}" ]] && kill -0 "${p}" 2>/dev/null && kill -TERM "${p}" 2>/dev/null && wait "${p}" 2>/dev/null
  done
  docker ps -aq --filter "label=com.jobctl.job_id" | xargs -r docker rm -f >/dev/null 2>&1
}
trap cleanup EXIT

start_controller() {
  JOBCTL_LISTEN=":${PORT}" \
  JOBCTL_DB="${DB}" \
  JOBCTL_WORKER_IMAGE="${WORKER_IMAGE}" \
  JOBCTL_HOST_STATE_DIR="${STATE_DIR}" \
  JOBCTL_GRACE_PERIOD="20s" \
    "${CONTROLLER_BIN}" >"$1" 2>&1 &
  echo $!
}

wait_health() {
  for _ in $(seq 1 50); do
    if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null; then return 0; fi
    sleep 0.2
  done
  echo "controller did not become healthy" >&2
  return 1
}

wait_checkpoints() {
  for _ in $(seq 1 600); do
    n=$(curl -sf "http://127.0.0.1:${PORT}/v1/jobs/$1" \
      | python3 -c "import json,sys; print(json.load(sys.stdin)['job'].get('last_checkpoint_epoch') or 0)")
    if [[ "${n}" -ge "$2" ]]; then return 0; fi
    sleep 0.2
  done
  return 1
}

wait_state() {
  local id="$1" want="$2" max="${3:-120}"
  local start
  start=$(date +%s)
  while true; do
    s=$(curl -sf "http://127.0.0.1:${PORT}/v1/jobs/${id}" \
      | python3 -c "import json,sys; print(json.load(sys.stdin)['job']['state'])" 2>/dev/null || true)
    if [[ "${s}" == "${want}" ]]; then return 0; fi
    now=$(date +%s)
    if (( now - start > max )); then return 1; fi
    sleep 0.5
  done
}

REF_STATE="${RUNTIME}/ref.bin"
"${ROOT}/worker/build/jobworker" --job-id ref --limit "${LIMIT}" \
  --checkpoint-every "${CHECKPOINT_EVERY}" --output-state "${REF_STATE}" \
  > "${RUNTIME}/ref.log"
REF_FOUND=$(grep -E '^\{"type":"completed"' "${RUNTIME}/ref.log" \
  | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["found"])')

CONTROLLER_PID=$(start_controller "${LOG}")
wait_health
JOB_ID=$(curl -sf -X POST "http://127.0.0.1:${PORT}/v1/jobs" \
  -H 'content-type: application/json' \
  -d "{\"limit\": ${LIMIT}, \"checkpoint_every\": ${CHECKPOINT_EVERY}, \"sleep_per_checkpoint_ms\": ${SLEEP_PER_CHECKPOINT_MS}}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

wait_checkpoints "${JOB_ID}" 2

echo "[chaos-sigterm] sending SIGTERM"
T_TERM=$(date +%s)
kill -TERM "${CONTROLLER_PID}"
wait "${CONTROLLER_PID}" 2>/dev/null || true
T_EXIT=$(date +%s)

CONTROLLER_PID2=$(start_controller "${LOG2}")
wait_health
wait_state "${JOB_ID}" "completed" 180
T_COMPLETE=$(date +%s)

JOB_FOUND=$(curl -sf "http://127.0.0.1:${PORT}/v1/jobs/${JOB_ID}" \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['job'].get('last_checkpoint_found'))")

JOB_STATE_PATH=$(find "${STATE_DIR}/${JOB_ID}" -name 'state.bin' -print -quit)
DETERMINISTIC=true
[[ "${JOB_FOUND}" != "${REF_FOUND}" ]] && DETERMINISTIC=false
[[ -n "${JOB_STATE_PATH}" && -f "${JOB_STATE_PATH}" ]] && ! cmp -s "${JOB_STATE_PATH}" "${REF_STATE}" && DETERMINISTIC=false

cat > "${RESULT}" <<JSON
{
  "signal": "SIGTERM",
  "kills": 0,
  "jobs": 1,
  "limit": ${LIMIT},
  "deterministic_match": ${DETERMINISTIC},
  "reference_found": ${REF_FOUND},
  "job_found": ${JOB_FOUND},
  "controller_exit_seconds": $((T_EXIT - T_TERM)),
  "post_term_completion_seconds": $((T_COMPLETE - T_TERM))
}
JSON

cat "${RESULT}"
[[ "${DETERMINISTIC}" == "true" ]] && echo "[chaos-sigterm] PASS" || { echo "[chaos-sigterm] FAIL" >&2; exit 1; }
