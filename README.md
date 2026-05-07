# job-controller

A fault-tolerant job controller for long-running CPU work on Linux. The
controller is a Go process; workers are C++ programs running inside Docker
containers. State lives in SQLite with WAL journaling. The crash-recovery
contract is verified by a chaos test that SIGKILLs the controller mid-job
and asserts the worker resumes to a byte-identical final state.

## What this studies

Three crash modes, and what each one is supposed to do:

| Crash mode                        | Outcome                                                |
| --------------------------------- | ------------------------------------------------------ |
| Controller dies, worker survives  | Restarted controller re-attaches via container labels and continues streaming checkpoints. |
| Both die, checkpoint exists       | Job is marked `interrupted_resumable`; the recovery scan schedules a resume that loads the persisted state file. |
| Both die, no checkpoint yet       | Job is marked `interrupted_unresumable`. Work is lost only because nothing was ever checkpointed. |

The failure surface is deliberately small. A single controller. A single
node. A single SQLite file. Every state transition is a transaction with
an explicit `wal_checkpoint`.

## Modules

| Path                        | Purpose                                                       |
| --------------------------- | ------------------------------------------------------------- |
| `cmd/controller`            | The supervisor process. Owns SQLite + the HTTP API.           |
| `cmd/reaper`                | CLI to find and remove orphaned worker containers.            |
| `internal/api`              | REST surface (`POST /v1/jobs`, list/get/cancel, `/healthz`).  |
| `internal/store`            | SQLite store, state machine, WAL discipline.                  |
| `internal/docker`           | `docker` CLI wrapper used by the supervisor.                  |
| `internal/supervisor`       | Per-job goroutine streaming worker stdout, persisting events. |
| `internal/signals`          | SIGTERM / SIGHUP / SIGUSR1 handlers.                          |
| `internal/recovery`         | Startup reconciliation between SQLite and Docker.             |
| `worker/`                   | C++20 worker (deterministic prime sieve + checkpoint I/O).    |
| `bench/chaos.sh`            | The crash test that the README's claims rest on.              |
| `infra/Dockerfile.worker`   | Multi-stage build: Debian builder → minimal runtime.          |
| `infra/Dockerfile.controller` | Multi-stage build for the controller image.                 |

## Quickstart

```bash
# Build
make build           # builds bin/controller, bin/reaper, worker/build/jobworker

# Tests
make test            # Go (-race) + GoogleTest via CTest

# Local end-to-end (requires a Docker daemon)
docker build -f infra/Dockerfile.worker -t jobctl/worker:dev .
JOBCTL_WORKER_IMAGE=jobctl/worker:dev bin/controller &
curl -s -X POST localhost:8080/v1/jobs \
  -H 'content-type: application/json' \
  -d '{"limit": 200000, "checkpoint_every": 2000}'
curl -s localhost:8080/v1/jobs | jq
```

## Chaos result

The committed `bench/chaos-result.json` is the artifact from a real local
run (`limit=2_000_000`, primes sieve to two million). The pass criterion
is `deterministic_match == true`: the worker's state file at completion
must be byte-identical to a non-crashed reference run with the same
parameters. The same script runs in CI on `ubuntu-latest` (with a smaller
`limit` for speed) on every push, and the CI artifact is uploaded under
the `chaos-result` artifact name.

```json
{
  "kills": 1,
  "jobs": 1,
  "limit": 300000,
  "checkpoint_every": 2000,
  "deterministic_match": true,
  "reference_found": 25997,
  "job_found": 25997,
  "worker_alive_after_kill": 1,
  "post_kill_completion_seconds": 5,
  "controller_recovery_seconds": 0,
  "wall_clock_seconds": 6
}
```

## Architecture (text diagram)

```
                        +-------------------+
                        |  HTTP API (8080)  |
                        |  /v1/jobs, /healthz
                        +---------+---------+
                                  |
                                  v
            +------------------------------------------+
            |             Supervisor                   |
            |  - per-job goroutine                     |
            |  - reads worker stdout (NDJSON)          |
            |  - persists checkpoints to SQLite        |
            |  - drives state machine transitions      |
            +-----+----------------------+-------------+
                  |                      |
                  v                      v
          +---------------+      +-------------------+
          | SQLite + WAL  |      |  Docker (CLI)     |
          | jobs/events   |      |  worker container |
          +---------------+      +---------+---------+
                                           |
                                           v
                                  +-------------------+
                                  |  C++ jobworker    |
                                  |  - prime sieve    |
                                  |  - checkpoints    |
                                  |    every K primes |
                                  +-------------------+
                                           |
                                           v
                                  +-------------------+
                                  |  state.bin (host) |
                                  |  CRC32-protected  |
                                  +-------------------+
```

## What this is *not*

- **Distributed.** Single controller; no leader election, no failover peer.
- **Exactly-once at network boundaries.** Only the local checkpoint
  protocol gives that guarantee, and only between the worker and SQLite.
- **Wired to real hardware-fault sources.** `SIGUSR1` is the simulated
  stand-in. Production wiring would consume `mcelog` or `/sys/devices/system/edac`.
- **GPU-aware.** No CUDA, no device plumbing.
- **Multi-tenant.** No auth, no per-tenant quotas, no priority queues, no
  work stealing.

See `ARCHITECTURE.md` for the state machine, the WAL discipline, the
re-attach protocol, and the determinism contract that the chaos test
relies on.

## License

MIT. See `LICENSE`.
