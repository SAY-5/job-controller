# Architecture

This document covers the load-bearing pieces: the job state machine, the
WAL discipline, the re-attach protocol, the determinism contract, and the
signal handlers (including the simulated hardware-fault stand-in).

## Job state machine

```
                +-----------+
                |  pending  |
                +-----+-----+
                      |
                      v
                +-----------+        +----------------------+
                |  running  +-----<--+  interrupted_        |
                +--+--------+        |    resumable         |
                   |                 +----------+-----------+
                   |                            ^
                   v                            |
        +----------------+                     |
        | checkpointing  +------+              |
        +-------+--------+      |              |
                |               |              |
                v               |              |
        +-------------+         |              |
        | completed   |         |              |
        +-------------+         |              |
                                |              |
                +-------------+ |              |
                |   failed    |<+              |
                +-------------+                |
                +-------------+                |
                |  cancelled  |                |
                +-------------+                |
                +-------------------------+    |
                | interrupted_unresumable |<---+
                +-------------------------+
```

The transitions are enforced in `internal/store/store.go`
(`validTransitions`). Self-transitions are accepted as no-ops to make
recovery idempotent.

| From                       | To                                           |
| -------------------------- | -------------------------------------------- |
| `pending`                  | `running`, `cancelled`, `failed`, `interrupted_unresumable` |
| `running`                  | `checkpointing`, `completed`, `failed`, `cancelled`, `interrupted_resumable`, `interrupted_unresumable` |
| `checkpointing`            | `running`, `completed`, `failed`, `cancelled`, `interrupted_resumable`, `interrupted_unresumable` |
| `interrupted_resumable`    | `running`, `cancelled`, `interrupted_unresumable` |
| terminal                   | self only (no-op)                            |

## WAL discipline

SQLite is opened with
`?_journal=WAL&_synchronous=NORMAL&_busy_timeout=5000` and a single
connection (`SetMaxOpenConns(1)`). Every state transition is wrapped in
`BEGIN; UPDATE jobs ...; INSERT INTO job_events ...; COMMIT;`. After
each commit:

- Non-terminal transition → `PRAGMA wal_checkpoint(PASSIVE)` (cheap; lets
  readers see the change without blocking writers).
- Terminal transition → `PRAGMA wal_checkpoint(FULL)` (truncates the WAL
  so the file size doesn't grow unbounded).
- On controller shutdown: `PRAGMA wal_checkpoint(FULL)` plus `Close()`.

The `wal_marker` table holds the timestamp of the most recent
controller-initiated WAL checkpoint and is exposed by `/healthz` so an
operator can detect a stuck checkpoint loop.

## Re-attach protocol

Worker containers carry three labels:

| Label                         | Value                                       |
| ----------------------------- | ------------------------------------------- |
| `com.jobctl.job_id`           | the job's UUID                              |
| `com.jobctl.controller_pid`   | OS pid of the controller that launched it   |
| `com.jobctl.created_at`       | unix-nano launch time                       |

On startup the recovery scan in `internal/recovery` lists every container
with `com.jobctl.job_id` set, joins it against the SQLite jobs table, and:

- If a job is `running` / `checkpointing` and a container with that
  job id is still running but tagged with a different
  `com.jobctl.controller_pid`, the supervisor adopts the container
  (`AdoptRunning`) and re-streams its logs.
- If the row is `running` but no live container exists, the job is moved
  to `interrupted_resumable` (assuming a checkpoint exists) or
  `interrupted_unresumable` (no checkpoint).
- If the row is `interrupted_resumable`, the supervisor schedules a
  resume by launching a new worker container with `--resume-from`
  pointing at the persisted state file.

## Determinism contract

The C++ worker is a streaming odd-only trial-division prime sieve. Given
fixed `(limit, checkpoint_every)` and a known initial state, every step
is deterministic: the same inputs produce the same `recent` ring, the
same `next` cursor, the same prime count, and therefore the same final
binary state file. The chaos test relies on this: it runs a reference
job to completion outside Docker, then runs the same job under the
controller, kills the controller mid-flight, restarts, and `cmp`s the
two final state files.

The state file format is documented in `worker/src/checkpoint.h`:

- 4-byte magic `JOBC` (`0x4A4F4243`) + 4-byte version
- Sieve cursor + counters (5 little-endian u64s, 1 u8 sentinel)
- Length-prefixed `recent` ring of recently-discovered primes
- Trailing CRC32 (IEEE 802.3, reflected) over all preceding bytes

The write path is atomic: write to `path.tmp`, `fsync`, `rename`. A
crash mid-write leaves a stale `.tmp` next to a still-valid `path`. The
read path verifies magic, version, and CRC; any structural problem
throws a typed `CheckpointError` and the resume is aborted with a
non-zero exit code.

## Signal handlers and the hardware-fault stand-in

| Signal     | Action                                                         |
| ---------- | -------------------------------------------------------------- |
| `SIGTERM`  | Broadcast `docker stop -t <grace>` to every tracked worker; wait up to `JOBCTL_GRACE_PERIOD` (default 30s); kill survivors. Mark non-terminal jobs `interrupted_resumable` if a checkpoint exists, `interrupted_unresumable` otherwise. Then exit. |
| `SIGINT`   | Same as `SIGTERM`.                                             |
| `SIGHUP`   | Reload config (re-read environment). Workers are not disturbed. |
| `SIGUSR1`  | **Simulated hardware fault.** Same shutdown path as `SIGTERM`, but jobs that have *not* yet checkpointed go straight to `interrupted_unresumable` (modeling "all in-flight work since the last checkpoint is gone"). |

`SIGUSR1` is a deliberate simulation. In a production deployment the
controller would subscribe to one of:

- `mcelog` / `mcelog --client`: legacy MCE delivery
- `/sys/devices/system/edac/mc/mc*`: kernel EDAC counters
- `rasdaemon` over D-Bus: modern uncorrected-MCE pipeline

We don't ship that wiring because it requires privileged access and
hardware that the CI runner doesn't have. The chaos test exercises the
SIGTERM and SIGKILL paths, which use the same shutdown plumbing.

## What's deliberately not here

- **No leader election.** A second controller pointed at the same SQLite
  file would corrupt the WAL.
- **No exactly-once at the API.** A `POST /v1/jobs` retry creates a
  fresh job; clients are expected to dedupe with their own request IDs.
- **No streaming job output.** The supervisor's NDJSON log is not
  exposed by the API; only metadata and recent events are.
- **No long-term metrics.** A Prometheus endpoint would be a clean next
  addition but isn't part of the present design.
- **No ACLs / multi-tenancy.** The single-controller assumption makes
  these meaningful only with an explicit operator story.

## File layout summary

```
cmd/controller/main.go     - process entry: load config, open store, scan, serve
cmd/reaper/main.go         - find/remove orphan containers
internal/api/api.go        - HTTP handlers + request id middleware
internal/store/store.go    - SQLite + state machine + WAL discipline
internal/store/schema.sql  - DDL (embedded into the binary)
internal/docker/client.go  - docker CLI wrapper (run/inspect/stop/ps/logs)
internal/supervisor/*      - per-job goroutine
internal/recovery/*        - startup reconciler
internal/signals/*         - SIGTERM / SIGHUP / SIGUSR1
worker/src/compute.cpp     - deterministic streaming sieve
worker/src/checkpoint.cpp  - CRC32 + length-prefix framing
worker/src/main.cpp        - CLI: --job-id, --limit, --checkpoint-every, --output-state, --resume-from
bench/chaos.sh             - the SIGKILL chaos test
bench/chaos-sigterm.sh     - the graceful-shutdown variant
```
