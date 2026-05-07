// Command concurrent_bench spawns N worker subprocesses concurrently and
// reports submit-to-complete latency percentiles, throughput, and harness
// resource usage. It is intentionally Docker-free so CI smoke runs do not
// need a daemon -- the C++ worker binary is invoked directly with the same
// flags the controller would pass.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	JobID    string        `json:"job_id"`
	LatNanos int64         `json:"latency_ns"`
	Found    int           `json:"found"`
	Err      string        `json:"err,omitempty"`
	Duration time.Duration `json:"-"`
}

type summary struct {
	NJobs              int     `json:"n_jobs"`
	PrimeLimit         int     `json:"prime_limit"`
	Concurrency        int     `json:"concurrency"`
	Successes          int     `json:"successes"`
	Failures           int     `json:"failures"`
	WallClockSec       float64 `json:"wall_clock_sec"`
	JobsPerSec         float64 `json:"jobs_per_sec"`
	LatencyP50Ms       float64 `json:"latency_p50_ms"`
	LatencyP95Ms       float64 `json:"latency_p95_ms"`
	LatencyP99Ms       float64 `json:"latency_p99_ms"`
	LatencyMaxMs       float64 `json:"latency_max_ms"`
	LatencyMeanMs      float64 `json:"latency_mean_ms"`
	HarnessMaxRSSMB    float64 `json:"harness_max_rss_mb"`
	HarnessNumGCBefore uint32  `json:"harness_num_gc_before"`
	HarnessNumGCAfter  uint32  `json:"harness_num_gc_after"`
	WorkerBinary       string  `json:"worker_binary"`
	Timestamp          string  `json:"timestamp"`
}

func main() {
	var (
		njobs    = flag.Int("n", 1000, "number of jobs to spawn")
		conc     = flag.Int("concurrency", 64, "max in-flight worker processes")
		limit    = flag.Int("limit", 10000, "per-job prime sieve limit")
		every    = flag.Int("checkpoint-every", 1000, "checkpoint interval (primes)")
		workerB  = flag.String("worker", "worker/build/jobworker", "worker binary path")
		outDir   = flag.String("out-dir", "bench/results", "output directory for the JSON result")
		failFast = flag.Bool("fail-fast", false, "abort on first worker failure")
	)
	flag.Parse()

	if _, err := os.Stat(*workerB); err != nil {
		fmt.Fprintf(os.Stderr, "worker binary %q not found: %v\n", *workerB, err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %q: %v\n", *outDir, err)
		os.Exit(2)
	}
	stateRoot, err := os.MkdirTemp("", "bench-state-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(2)
	}
	defer os.RemoveAll(stateRoot)

	var msBefore runtime.MemStats
	runtime.ReadMemStats(&msBefore)

	results := make([]result, *njobs)
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	var failed atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wallStart := time.Now()
	for i := 0; i < *njobs; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if *failFast && failed.Load() > 0 {
				results[idx].Err = "skipped"
				return
			}
			jobID := fmt.Sprintf("bench-%06d", idx)
			statePath := filepath.Join(stateRoot, jobID+".bin")
			start := time.Now()
			cmd := exec.CommandContext(ctx, *workerB,
				"--job-id", jobID,
				"--limit", fmt.Sprintf("%d", *limit),
				"--checkpoint-every", fmt.Sprintf("%d", *every),
				"--output-state", statePath,
			)
			out, err := cmd.Output()
			lat := time.Since(start)
			r := result{JobID: jobID, LatNanos: lat.Nanoseconds(), Duration: lat}
			if err != nil {
				r.Err = err.Error()
				failed.Add(1)
			} else {
				// Pull `found` from the last "completed" event line.
				r.Found = parseFound(out)
			}
			results[idx] = r
		}(i)
	}
	wg.Wait()
	wall := time.Since(wallStart)

	var msAfter runtime.MemStats
	runtime.ReadMemStats(&msAfter)

	// Build summary.
	lats := make([]float64, 0, *njobs)
	successes := 0
	failures := 0
	for _, r := range results {
		if r.Err == "" {
			successes++
			lats = append(lats, float64(r.LatNanos)/1e6)
		} else {
			failures++
		}
	}
	sort.Float64s(lats)
	mean := 0.0
	for _, v := range lats {
		mean += v
	}
	if len(lats) > 0 {
		mean /= float64(len(lats))
	}
	sum := summary{
		NJobs:              *njobs,
		PrimeLimit:         *limit,
		Concurrency:        *conc,
		Successes:          successes,
		Failures:           failures,
		WallClockSec:       wall.Seconds(),
		JobsPerSec:         float64(successes) / wall.Seconds(),
		LatencyP50Ms:       percentile(lats, 0.50),
		LatencyP95Ms:       percentile(lats, 0.95),
		LatencyP99Ms:       percentile(lats, 0.99),
		LatencyMaxMs:       maxF(lats),
		LatencyMeanMs:      mean,
		HarnessMaxRSSMB:    float64(msAfter.Sys) / (1024 * 1024),
		HarnessNumGCBefore: msBefore.NumGC,
		HarnessNumGCAfter:  msAfter.NumGC,
		WorkerBinary:       *workerB,
		Timestamp:          time.Now().UTC().Format(time.RFC3339),
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	outPath := filepath.Join(*outDir, ts+".json")
	if err := writeJSON(outPath, sum); err != nil {
		fmt.Fprintf(os.Stderr, "write result: %v\n", err)
		os.Exit(2)
	}
	// Also write a "latest.json" pointer for the bench-regress gate.
	_ = writeJSON(filepath.Join(*outDir, "latest.json"), sum)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(sum)
	fmt.Fprintf(os.Stderr, "result -> %s\n", outPath)

	if failures > 0 {
		os.Exit(1)
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := p * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rank-float64(lo))
}

func maxF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, v := range xs[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func writeJSON(path string, body any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseFound(out []byte) int {
	// The last "completed" line in stdout looks like:
	//   {"type":"completed","job_id":"...","found":N,"epoch":E}
	// We avoid pulling encoding/json into the hot path by just locating the
	// `"found":` substring near the tail. Best-effort; bench correctness
	// does not depend on this value.
	const key = `"found":`
	last := -1
	for i := 0; i+len(key) < len(out); i++ {
		if string(out[i:i+len(key)]) == key {
			last = i + len(key)
		}
	}
	if last < 0 {
		return 0
	}
	v := 0
	for i := last; i < len(out); i++ {
		c := out[i]
		if c >= '0' && c <= '9' {
			v = v*10 + int(c-'0')
		} else {
			break
		}
	}
	return v
}
