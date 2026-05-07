package registry_test

// Chaos-recovery integration tests for the three bundled workers. These
// run the jobworker binary directly (no Docker), kill it mid-run via
// SIGKILL, then resume from the persisted state file and assert the
// resumed state matches a pristine reference run byte-for-byte.
//
// Skipped when the worker binary is missing (e.g. when running pure unit
// tests without a CMake build).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	checkpointEvery = 10
)

func findWorkerBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "worker", "build", "jobworker"),
		filepath.Join("..", "..", "..", "worker", "build", "jobworker"),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	t.Skip("jobworker binary not built; run `make build-cpp` first")
	return ""
}

type chaosRun struct {
	worker          string
	limit           int64
	seed            int64
	checkpointEvery int
	sleepMs         int
}

// runWorker spawns jobworker once and lets it complete. Returns the path
// to the final state file.
func runWorker(t *testing.T, bin string, run chaosRun, statePath string, resumeFrom string) error {
	t.Helper()
	args := []string{
		"--worker", run.worker,
		"--job-id", "chaos-" + run.worker,
		"--limit", fmt.Sprintf("%d", run.limit),
		"--seed", fmt.Sprintf("%d", run.seed),
		"--checkpoint-every", fmt.Sprintf("%d", run.checkpointEvery),
		"--output-state", statePath,
	}
	if run.sleepMs > 0 {
		args = append(args, "--sleep-per-checkpoint-ms", fmt.Sprintf("%d", run.sleepMs))
	}
	if resumeFrom != "" {
		args = append(args, "--resume-from", resumeFrom)
	}
	cmd := exec.Command(bin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: stderr=%s", err, errOut.String())
	}
	return nil
}

// runUntilKill spawns the worker, waits for the requested number of
// checkpoint events on stdout, then SIGKILLs it. Returns once the
// process has reaped.
func runUntilKill(t *testing.T, bin string, run chaosRun, statePath string, killAfterCheckpoints int) {
	t.Helper()
	args := []string{
		"--worker", run.worker,
		"--job-id", "chaos-" + run.worker,
		"--limit", fmt.Sprintf("%d", run.limit),
		"--seed", fmt.Sprintf("%d", run.seed),
		"--checkpoint-every", fmt.Sprintf("%d", run.checkpointEvery),
		"--output-state", statePath,
	}
	if run.sleepMs > 0 {
		args = append(args, "--sleep-per-checkpoint-ms", fmt.Sprintf("%d", run.sleepMs))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if runtime.GOOS == "linux" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// Stream stdout in a goroutine; the test side polls a checkpoint counter.
	type token struct {
		count int
	}
	ch := make(chan token, 64)
	go func() {
		defer close(ch)
		var streambuf bytes.Buffer
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				streambuf.Write(buf[:n])
				count := strings.Count(streambuf.String(), `"type":"checkpoint"`)
				ch <- token{count: count}
			}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	seen := 0
	for time.Now().Before(deadline) {
		select {
		case tk, ok := <-ch:
			if !ok {
				if seen < killAfterCheckpoints {
					t.Fatalf("worker exited early: saw %d/%d checkpoints", seen, killAfterCheckpoints)
				}
				return
			}
			seen = tk.count
			if seen >= killAfterCheckpoints {
				_ = cmd.Process.Kill()
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	t.Fatalf("did not see %d checkpoints within deadline (saw %d)", killAfterCheckpoints, seen)
}

func TestChaosRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chaos-recovery integration in -short mode")
	}
	bin := findWorkerBinary(t)

	cases := []chaosRun{
		{worker: "primes", limit: 5000, seed: 1, checkpointEvery: 100, sleepMs: 5},
		{worker: "matmul", limit: 30, seed: 7, checkpointEvery: 50, sleepMs: 5},
		{worker: "wordcount", limit: 5000, seed: 13, checkpointEvery: 250, sleepMs: 5},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.worker, func(t *testing.T) {
			dir := t.TempDir()
			refPath := filepath.Join(dir, "ref.bin")
			runPath := filepath.Join(dir, "job.bin")

			// Reference run (no kill).
			if err := runWorker(t, bin, tc, refPath, ""); err != nil {
				t.Fatalf("reference run: %v", err)
			}

			// Chaos run: kill after a few checkpoints, then resume.
			runUntilKill(t, bin, tc, runPath, 3)

			// Resume from the persisted state file.
			if err := runWorker(t, bin, tc, runPath, runPath); err != nil {
				t.Fatalf("resume run: %v", err)
			}

			// Compare bytes.
			refBytes, err := os.ReadFile(refPath)
			if err != nil {
				t.Fatalf("read ref: %v", err)
			}
			runBytes, err := os.ReadFile(runPath)
			if err != nil {
				t.Fatalf("read run: %v", err)
			}
			if !bytes.Equal(refBytes, runBytes) {
				t.Fatalf("[%s] resumed state differs from reference (ref=%d bytes, run=%d bytes)",
					tc.worker, len(refBytes), len(runBytes))
			}
			t.Logf("[%s] chaos-recovery byte-identical match (%d bytes)", tc.worker, len(refBytes))
		})
	}
}
