# Chaos tests

Two scripts that exercise the crash/recovery contract end to end. Both
require:

- a built C++ worker (`worker/build/jobworker`)
- a built controller binary (`bin/controller`)
- a running Docker daemon, with the worker image tagged `jobctl/worker:dev`
- Python 3 (used for tiny JSON parsing in the harness)

## chaos.sh — SIGKILL the controller mid-job

1. Run a reference (non-crash) job to get the deterministic answer.
2. Boot the controller and submit the same job through the API.
3. Wait for at least 3 checkpoints to be persisted.
4. `kill -9` the controller PID.
5. Confirm the worker container survives the controller crash.
6. Boot a second controller. The recovery scan must re-attach via labels
   and resume log streaming.
7. Wait for the job to reach `completed` and compare the worker's state
   file (and `last_checkpoint_found`) against the reference run.

The result is written to `bench/runtime/chaos-result.json`.

## chaos-sigterm.sh — graceful shutdown then resume

Same shape but the controller receives SIGTERM. The worker should
checkpoint and exit cleanly, the controller should exit within the grace
period, and a fresh boot should resume the job from the last checkpoint.

## What "deterministic_match" actually means

Given fixed `(limit, checkpoint_every)`, the prime sieve is a pure
function. A successful resume must produce a byte-identical state file at
completion. The harness `cmp -s`s the worker's final state file against
the reference run's, so any drift (e.g. double-counting, off-by-one on the
sieve cursor) is caught.
