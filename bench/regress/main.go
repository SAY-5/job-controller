// Command bench_regress compares the most recent bench result against the
// committed bench/baseline.json floor. Exits non-zero if any metric
// violates the floor. Intended to wire into CI as `make bench-regress`.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

type baseline struct {
	MinJobsPerSec float64 `json:"min_jobs_per_sec"`
	MaxP50Ms      float64 `json:"max_p50_ms"`
	MaxP95Ms      float64 `json:"max_p95_ms"`
	MaxP99Ms      float64 `json:"max_p99_ms"`
	MaxFailures   int     `json:"max_failures"`
}

type latest struct {
	NJobs        int     `json:"n_jobs"`
	Successes    int     `json:"successes"`
	Failures     int     `json:"failures"`
	JobsPerSec   float64 `json:"jobs_per_sec"`
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP95Ms float64 `json:"latency_p95_ms"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`
}

func main() {
	var (
		baselinePath = flag.String("baseline", "bench/baseline.json", "path to the committed baseline floor")
		latestPath   = flag.String("latest", "bench/results/latest.json", "path to the most recent bench result")
	)
	flag.Parse()

	var bl baseline
	if err := readJSON(*baselinePath, &bl); err != nil {
		fail("read baseline %s: %v", *baselinePath, err)
	}
	var lt latest
	if err := readJSON(*latestPath, &lt); err != nil {
		fail("read latest %s: %v", *latestPath, err)
	}

	violations := []string{}
	if lt.Failures > bl.MaxFailures {
		violations = append(violations, fmt.Sprintf("failures=%d > max=%d", lt.Failures, bl.MaxFailures))
	}
	if bl.MinJobsPerSec > 0 && lt.JobsPerSec < bl.MinJobsPerSec {
		violations = append(violations, fmt.Sprintf("jobs_per_sec=%.2f < min=%.2f", lt.JobsPerSec, bl.MinJobsPerSec))
	}
	if bl.MaxP50Ms > 0 && lt.LatencyP50Ms > bl.MaxP50Ms {
		violations = append(violations, fmt.Sprintf("p50=%.2fms > max=%.2fms", lt.LatencyP50Ms, bl.MaxP50Ms))
	}
	if bl.MaxP95Ms > 0 && lt.LatencyP95Ms > bl.MaxP95Ms {
		violations = append(violations, fmt.Sprintf("p95=%.2fms > max=%.2fms", lt.LatencyP95Ms, bl.MaxP95Ms))
	}
	if bl.MaxP99Ms > 0 && lt.LatencyP99Ms > bl.MaxP99Ms {
		violations = append(violations, fmt.Sprintf("p99=%.2fms > max=%.2fms", lt.LatencyP99Ms, bl.MaxP99Ms))
	}

	fmt.Printf("[bench-regress] n=%d successes=%d failures=%d throughput=%.2f j/s p50=%.2fms p95=%.2fms p99=%.2fms\n",
		lt.NJobs, lt.Successes, lt.Failures, lt.JobsPerSec, lt.LatencyP50Ms, lt.LatencyP95Ms, lt.LatencyP99Ms)

	if len(violations) > 0 {
		fmt.Fprintf(os.Stderr, "[bench-regress] FAIL:\n")
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		os.Exit(1)
	}
	fmt.Println("[bench-regress] PASS")
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[bench-regress] "+format+"\n", a...)
	os.Exit(2)
}
