package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTmp(t *testing.T, dir, name string, body any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestBaselineParses(t *testing.T) {
	dir := t.TempDir()
	p := writeTmp(t, dir, "b.json", baseline{
		MinJobsPerSec: 100,
		MaxP50Ms:      50,
		MaxP95Ms:      200,
		MaxP99Ms:      500,
		MaxFailures:   0,
	})
	var got baseline
	if err := readJSON(p, &got); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if got.MinJobsPerSec != 100 || got.MaxP95Ms != 200 {
		t.Fatalf("baseline mismatch: %+v", got)
	}
}

func TestLatestParses(t *testing.T) {
	dir := t.TempDir()
	p := writeTmp(t, dir, "l.json", latest{
		NJobs:        20,
		Successes:    20,
		Failures:     0,
		JobsPerSec:   500,
		LatencyP50Ms: 5,
		LatencyP95Ms: 12,
		LatencyP99Ms: 18,
	})
	var got latest
	if err := readJSON(p, &got); err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if got.NJobs != 20 || got.JobsPerSec != 500 {
		t.Fatalf("latest mismatch: %+v", got)
	}
}
