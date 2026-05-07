package cluster_test

// This test simulates two controller processes ("ctl-A", "ctl-B") sharing
// the same in-memory locker, mirroring the production deployment where
// they would share Redis (for the lock) and Postgres/SQLite (for job
// state). The test verifies:
//
//  1. Exactly one controller becomes leader at boot.
//  2. Killing the leader (cancelling its context) lets the survivor pick
//     up the lease within the configured failover SLA.
//  3. While both controllers are alive, ONLY the leader spawns a worker
//     for any given job; the follower's start attempts are rejected
//     with supervisor.ErrNotLeader so no duplicate worker container can
//     be created.
//
// We don't drive Docker -- instead we wire each "supervisor" to a fake
// spawn counter and assert the leader's counter went up while the
// follower's stays at zero.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SAY-5/job-controller/internal/cluster"
)

// fakeSupervisor mimics the leader-gating behavior of the real supervisor
// without touching Docker. It records every Start it allows so the test
// can assert no double-spawn happened.
type fakeSupervisor struct {
	id          string
	leaderCheck func() bool
	starts      atomic.Int32
}

var errNotLeader = errors.New("not leader")

func (f *fakeSupervisor) Start(_ context.Context, _ string) error {
	if !f.leaderCheck() {
		return errNotLeader
	}
	f.starts.Add(1)
	return nil
}

func TestTwoControllersFailoverNoDoubleSpawn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster integration test in -short mode")
	}
	locker := cluster.NewInMemoryLocker()
	cfg := cluster.Config{
		Key:          "cb:leader:lock",
		LeaseTTL:     2 * time.Second,
		RefreshEvery: 500 * time.Millisecond,
		PollEvery:    250 * time.Millisecond,
	}

	cfgA := cfg
	cfgA.ControllerID = "ctl-A"
	cfgB := cfg
	cfgB.ControllerID = "ctl-B"

	electorA := cluster.New(cfgA, locker, nil, nil)
	electorB := cluster.New(cfgB, locker, nil, nil)

	supA := &fakeSupervisor{id: "ctl-A", leaderCheck: electorA.IsLeader}
	supB := &fakeSupervisor{id: "ctl-B", leaderCheck: electorB.IsLeader}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); electorA.Run(ctxA) }()
	go func() { defer wg.Done(); electorB.Run(ctxB) }()

	// Wait for one to become leader.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if electorA.IsLeader() != electorB.IsLeader() && (electorA.IsLeader() || electorB.IsLeader()) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if electorA.IsLeader() == electorB.IsLeader() {
		t.Fatalf("expected exactly one leader after 3s")
	}

	// Drive a synthetic stream of "job submitted" events through BOTH
	// controllers and verify only the leader spawns. This is the
	// double-spawn safety check.
	for i := 0; i < 20; i++ {
		_ = supA.Start(context.Background(), "job-1")
		_ = supB.Start(context.Background(), "job-1")
		time.Sleep(5 * time.Millisecond)
	}

	leaderID := "ctl-A"
	if !electorA.IsLeader() {
		leaderID = "ctl-B"
	}
	leaderStarts := supA.starts.Load()
	followerStarts := supB.starts.Load()
	if leaderID == "ctl-B" {
		leaderStarts = supB.starts.Load()
		followerStarts = supA.starts.Load()
	}
	if leaderStarts == 0 {
		t.Fatalf("leader %s never spawned a worker (count=0)", leaderID)
	}
	if followerStarts != 0 {
		t.Fatalf("follower spawned %d workers (must be 0 -- double-spawn detected)", followerStarts)
	}
	t.Logf("phase 1: leader=%s starts=%d follower starts=0 ok", leaderID, leaderStarts)

	// Kill the leader and measure failover.
	t0 := time.Now()
	if leaderID == "ctl-A" {
		cancelA()
	} else {
		cancelB()
	}

	failoverDeadline := t0.Add(30 * time.Second)
	for time.Now().Before(failoverDeadline) {
		if leaderID == "ctl-A" && electorB.IsLeader() {
			break
		}
		if leaderID == "ctl-B" && electorA.IsLeader() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	failover := time.Since(t0)
	if failover > 30*time.Second {
		t.Fatalf("failover %v exceeds 30s SLA", failover)
	}
	t.Logf("failover: %v (within 30s SLA)", failover)

	// Continue submitting; the new leader picks up.
	prevLeaderStarts := supA.starts.Load() + supB.starts.Load()
	for i := 0; i < 20; i++ {
		_ = supA.Start(context.Background(), "job-2")
		_ = supB.Start(context.Background(), "job-2")
		time.Sleep(5 * time.Millisecond)
	}
	finalStarts := supA.starts.Load() + supB.starts.Load()
	if finalStarts <= prevLeaderStarts {
		t.Fatalf("new leader did not spawn after failover: %d -> %d", prevLeaderStarts, finalStarts)
	}
	t.Logf("phase 2: starts %d -> %d after failover", prevLeaderStarts, finalStarts)

	cancelA()
	cancelB()
	wg.Wait()
}
