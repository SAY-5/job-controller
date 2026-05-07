package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGetJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	job := Job{
		ID:      "job-1",
		Image:   "alpine:3",
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hi"},
	}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StatePending {
		t.Fatalf("state = %s want pending", got.State)
	}
	if got.Image != "alpine:3" {
		t.Fatalf("image = %s", got.Image)
	}
	if len(got.Args) != 2 || got.Args[1] != "echo hi" {
		t.Fatalf("args = %v", got.Args)
	}
}

func TestTransitionsAreValidated(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, Job{ID: "j1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "j1", StateRunning); err != nil {
		t.Fatalf("pending->running: %v", err)
	}
	// Going from running directly back to pending is illegal.
	if err := s.Transition(ctx, "j1", StatePending); err == nil {
		t.Fatalf("running->pending should be invalid")
	}
	if err := s.Transition(ctx, "j1", StateCompleted); err != nil {
		t.Fatalf("running->completed: %v", err)
	}
	// Idempotent self-transition on terminal state.
	if err := s.Transition(ctx, "j1", StateCompleted); err != nil {
		t.Fatalf("completed->completed should be no-op: %v", err)
	}
}

func TestRecordCheckpointAdvancesMetadata(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, Job{ID: "j1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckpoint(ctx, "j1", 5, 100, "/state/state.bin"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCheckpointEpoch == nil || *got.LastCheckpointEpoch != 5 {
		t.Fatalf("epoch = %v", got.LastCheckpointEpoch)
	}
	if got.LastCheckpointFound == nil || *got.LastCheckpointFound != 100 {
		t.Fatalf("found = %v", got.LastCheckpointFound)
	}
	if got.LastCheckpointPath == nil || *got.LastCheckpointPath != "/state/state.bin" {
		t.Fatalf("path = %v", got.LastCheckpointPath)
	}
}

func TestListJobsCursorPagination(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		j := Job{
			ID:        fakeID(i),
			Image:     "img",
			CreatedAt: time.Unix(0, int64(1_000_000_000+i*1_000_000)),
		}
		if err := s.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	page1, cursor, err := s.ListJobs(ctx, "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 size = %d", len(page1))
	}
	if cursor == "" {
		t.Fatal("expected cursor for next page")
	}
	page2, cursor2, err := s.ListJobs(ctx, "", cursor, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 3 {
		t.Fatalf("page2 size = %d", len(page2))
	}
	// All 6 IDs across the two pages must be unique.
	seen := make(map[string]bool)
	for _, j := range append(page1, page2...) {
		if seen[j.ID] {
			t.Fatalf("duplicate id %s across pages", j.ID)
		}
		seen[j.ID] = true
	}
	page3, _, err := s.ListJobs(ctx, "", cursor2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 size = %d (want 1)", len(page3))
	}
}

func TestRecoveryScanReturnsRunningAndCheckpointing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.CreateJob(ctx, Job{ID: id, Image: "img"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Transition(ctx, "a", StateRunning)
	_ = s.Transition(ctx, "b", StateRunning)
	_ = s.Transition(ctx, "b", StateCheckpointing)
	_ = s.Transition(ctx, "c", StateRunning)
	_ = s.Transition(ctx, "c", StateCompleted)

	jobs, err := s.JobsInStates(ctx, StateRunning, StateCheckpointing)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	got := map[string]State{}
	for _, j := range jobs {
		got[j.ID] = j.State
	}
	if got["a"] != StateRunning || got["b"] != StateCheckpointing {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestRecentEventsAreOrdered(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateJob(ctx, Job{ID: "j1", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Transition(ctx, "j1", StateRunning)
	_ = s.RecordCheckpoint(ctx, "j1", 1, 10, "/x")
	_ = s.RecordCheckpoint(ctx, "j1", 2, 20, "/x")
	events, err := s.RecentEvents(ctx, "j1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("event count = %d", len(events))
	}
	// Newest first.
	for i := 1; i < len(events); i++ {
		if events[i-1].ID < events[i].ID {
			t.Fatalf("not sorted: %d before %d", events[i-1].ID, events[i].ID)
		}
	}
}

func fakeID(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return string(letters[i]) + "0"
}
