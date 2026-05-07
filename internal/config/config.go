// Package config holds runtime knobs for the controller. Values are sourced
// from environment variables with conservative defaults so the binary works
// out of the box for the chaos test.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Listen address for the HTTP API.
	Listen string

	// Path to the SQLite database file. WAL mode is enabled in store.Open.
	DBPath string

	// Default Docker image used when a job omits one.
	DefaultImage string

	// Path inside the worker container where the state file is materialized.
	WorkerStatePath string

	// Host-side directory holding per-job state files. Mounted into workers.
	HostStateDir string

	// Grace period when sending SIGTERM to worker containers.
	GracePeriod time.Duration

	// How often the supervisor sweeps for orphan / crashed workers.
	ReconcileEvery time.Duration

	// Whether to skip Docker (used by unit tests; the real binary keeps it false).
	NoDocker bool
}

func Load() Config {
	return Config{
		Listen:          envOr("JOBCTL_LISTEN", ":8080"),
		DBPath:          envOr("JOBCTL_DB", "/var/lib/jobctl/jobs.db"),
		DefaultImage:    envOr("JOBCTL_WORKER_IMAGE", "jobctl/worker:latest"),
		WorkerStatePath: envOr("JOBCTL_WORKER_STATE_PATH", "/state/state.bin"),
		HostStateDir:    envOr("JOBCTL_HOST_STATE_DIR", "/var/lib/jobctl/state"),
		GracePeriod:     envDur("JOBCTL_GRACE_PERIOD", 30*time.Second),
		ReconcileEvery:  envDur("JOBCTL_RECONCILE_EVERY", 10*time.Second),
		NoDocker:        envBool("JOBCTL_NO_DOCKER", false),
	}
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: invalid duration for %s: %v\n", k, err)
		return def
	}
	return d
}

func envBool(k string, def bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
