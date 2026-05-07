#!/usr/bin/env bash
# Chaos test: SIGKILL the controller mid-job, restart, assert the worker
# resumes from its last checkpoint and produces the same final output as a
# non-crashed reference run with the same parameters.
#
# Required env: docker daemon reachable; jobctl/worker image already built.
# Optional env: CHAOS_LIMIT (default 1000000), CHAOS_CHECKPOINT_EVERY_PRIMES
# (default 5000).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUNTIME="${ROOT}/bench/runtime"
CONTROLLER_BIN="${ROOT}/bin/controller"
WORKER_IMAGE="${WORKER_IMAGE:-jobctl/worker:dev}"
LIMIT="${CHAOS_LIMIT:-1000000}"
CHECKPOINT_EVERY="${CHAOS_CHECKPOINT_EVERY_PRIMES:-5000}"
# Each checkpoint emission sleeps this many ms. With default 50 and ~50
# checkpoints, the worker runs for ~2.5s after the chaos kill, giving the
# recovery scan time to re-attach and observe a non-completed job.
SLEEP_PER_CHECKPOINT_MS="${CHAOS_SLEEP_PER_CHECKPOINT_MS:-50}"
PORT="${PORT:-8089}"

mkdir -p "${RUNTIME}"
rm -rf "${RUNTIME:?}/"*

DB="${RUNTIME}/jobs.db"
STATE_DIR="${RUNTIME}/state"
LOG="${RUNTIME}/controller.log"
LOG2="${RUNTIME}/controller.restart.log"
RESULT="${RUNTIME}/chaos-result.json"
mkdir -p "${STATE_DIR}"

cleanup() {
  set +e
  if [[ -n "${CONTROLLER_PID:-}" ]] && kill -0 "${CONTROLLER_PID}" 2>/dev/null; then
    kill -TERM "${CONTROLLER_PID}" 2>/dev/null
    wait "${CONTROLLER_PID}" 2>/dev/null
  fi
  if [[ -n "${CONTROLLER_PID2:-}" ]] && kill -0 "${CONTROLLER_PID2}" 2>/dev/null; then
    kill -TERM "${CONTROLLER_PID2}" 2>/dev/null
    wait "${CONTROLLER_PID2}" 2>/dev/null
  fi
  # Best-effort: remove orphan worker containers from this run.
  docker ps -aq --filter "label=com.jobctl.job_id" | xargs -r docker rm -f >/dev/null 2>&1
}
trap cleanup EXIT

start_controller() {
  local logfile="$1"
  JOBCTL_LISTEN=":${PORT}" \
  JOBCTL_DB="${DB}" \
  JOBCTL_WORKER_IMAGE="${WORKER_IMAGE}" \
  JOBCTL_HOST_STATE_DIR="${STATE_DIR}" \
  JOBCTL_GRACE_PERIOD="20s" \
    "${CONTROLLER_BIN}" >"${logfile}" 2>&1 &
  echo $!
}

wait_health() {
  local pid="$1"
  for _ in $(seq 1 50); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "controller pid ${pid} died early; log follows:" >&2
      cat "${LOG}" >&2 || true
      exit 1
    fi
    if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null; then return 0; fi
    sleep 0.2
  done
  echo "controller did not become healthy" >&2
  return 1
}

submit_job() {
  curl -sf -X POST "http://127.0.0.1:${PORT}/v1/jobs" \
    -H 'content-type: application/json' \
    -d "{\"limit\": ${LIMIT}, \"checkpoint_every\": ${CHECKPOINT_EVERY}, \"sleep_per_checkpoint_ms\": ${SLEEP_PER_CHECKPOINT_MS}}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}

job_state() {
  curl -sf "http://127.0.0.1:${PORT}/v1/jobs/$1" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["job"]["state"])'
}

job_field() {
  curl -sf "http://127.0.0.1:${PORT}/v1/jobs/$1" \
    | python3 -c "import json,sys; d=json.load(sys.stdin)['job']; v=d.get('$2'); print(v if v is not None else '')"
}

wait_checkpoints() {
  local id="$1" needed="$2"
  for _ in $(seq 1 600); do
    local n
    n=$(job_field "${id}" last_checkpoint_epoch)
    if [[ -n "${n}" && "${n}" -ge "${needed}" ]]; then return 0; fi
    sleep 0.2
  done
  echo "did not see ${needed} checkpoints" >&2
  return 1
}

wait_state() {
  local id="$1" want="$2" max_seconds="${3:-120}"
  local start now
  start=$(date +%s)
  while true; do
    local s
    s=$(job_state "${id}" 2>/dev/null || true)
    if [[ "${s}" == "${want}" ]]; then return 0; fi
    now=$(date +%s)
    if (( now - start > max_seconds )); then
      echo "timeout waiting for state ${want} (last=${s})" >&2
      return 1
    fi
    sleep 0.5
  done
}

# Reference run: same parameters, no crash. Computes the deterministic answer.
echo "[chaos] reference run: limit=${LIMIT} ckpt_every=${CHECKPOINT_EVERY}"
REF_STATE="${RUNTIME}/ref.bin"
"${ROOT}/worker/build/jobworker" --job-id ref --limit "${LIMIT}" \
  --checkpoint-every "${CHECKPOINT_EVERY}" --output-state "${REF_STATE}" \
  > "${RUNTIME}/ref.log"
REF_FOUND=$(grep -E '^\{"type":"completed"' "${RUNTIME}/ref.log" \
  | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["found"])')
echo "[chaos] reference found=${REF_FOUND}"

# First controller boot.
echo "[chaos] launching controller (boot 1)"
CONTROLLER_PID=$(start_controller "${LOG}")
wait_health "${CONTROLLER_PID}"

T_SUBMIT=$(date +%s)
JOB_ID=$(submit_job)
echo "[chaos] submitted job=${JOB_ID}"

echo "[chaos] waiting for >=3 checkpoints"
wait_checkpoints "${JOB_ID}" 3

# Verify the worker container is still running (chaos requires job not done yet).
if ! docker ps --format '{{.Labels}}' | grep -F "com.jobctl.job_id=${JOB_ID}" >/dev/null; then
  echo "[chaos] FAIL: worker exited before kill (job too short)" >&2
  exit 1
fi

echo "[chaos] SIGKILL controller pid=${CONTROLLER_PID}"
T_KILL=$(date +%s)
kill -9 "${CONTROLLER_PID}"
wait "${CONTROLLER_PID}" 2>/dev/null || true

# Verify the worker container survived the controller crash.
ALIVE_BEFORE_RESTART=0
if docker ps --format '{{.Labels}}' | grep -F "com.jobctl.job_id=${JOB_ID}" >/dev/null; then
  ALIVE_BEFORE_RESTART=1
fi

sleep 5

echo "[chaos] launching controller (boot 2)"
T_RESTART=$(date +%s)
CONTROLLER_PID2=$(start_controller "${LOG2}")
wait_health "${CONTROLLER_PID2}"
T_HEALTHY=$(date +%s)

echo "[chaos] waiting for completion"
wait_state "${JOB_ID}" "completed" 180
T_COMPLETE=$(date +%s)

# Compare the persisted worker state against the reference state.
JOB_STATE_PATH=$(find "${STATE_DIR}/${JOB_ID}" -name 'state.bin' -print -quit)
DETERMINISTIC=true
JOB_FOUND=$(job_field "${JOB_ID}" last_checkpoint_found)
if [[ "${JOB_FOUND}" != "${REF_FOUND}" ]]; then DETERMINISTIC=false; fi
if [[ -n "${JOB_STATE_PATH}" && -f "${JOB_STATE_PATH}" ]]; then
  if ! cmp -s "${JOB_STATE_PATH}" "${REF_STATE}"; then DETERMINISTIC=false; fi
fi

cat > "${RESULT}" <<JSON
{
  "kills": 1,
  "jobs": 1,
  "limit": ${LIMIT},
  "checkpoint_every": ${CHECKPOINT_EVERY},
  "deterministic_match": ${DETERMINISTIC},
  "reference_found": ${REF_FOUND},
  "job_found": ${JOB_FOUND:-null},
  "worker_alive_after_kill": ${ALIVE_BEFORE_RESTART},
  "post_kill_completion_seconds": $((T_COMPLETE - T_KILL)),
  "controller_recovery_seconds": $((T_HEALTHY - T_RESTART)),
  "wall_clock_seconds": $((T_COMPLETE - T_SUBMIT))
}
JSON

echo "[chaos] result:"
cat "${RESULT}"

if [[ "${DETERMINISTIC}" != "true" ]]; then
  echo "[chaos] FAIL: outputs diverged" >&2
  exit 1
fi
if [[ "${ALIVE_BEFORE_RESTART}" != "1" ]]; then
  echo "[chaos] FAIL: worker did not survive controller kill" >&2
  exit 1
fi
echo "[chaos] PASS"
