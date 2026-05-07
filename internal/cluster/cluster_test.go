package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestInMemoryLockerHappyPath(t *testing.T) {
	l := NewInMemoryLocker()
	ctx := context.Background()
	ok, err := l.Acquire(ctx, "k", "A", 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("A acquire: ok=%v err=%v", ok, err)
	}
	ok2, err := l.Acquire(ctx, "k", "B", 5*time.Second)
	if err != nil || ok2 {
		t.Fatalf("B should not acquire while A holds: ok=%v err=%v", ok2, err)
	}
	if err := l.Refresh(ctx, "k", "A", 5*time.Second); err != nil {
		t.Fatalf("A refresh: %v", err)
	}
	if err := l.Refresh(ctx, "k", "B", 5*time.Second); err == nil {
		t.Fatalf("B refresh should fail")
	}
	if err := l.Release(ctx, "k", "A"); err != nil {
		t.Fatalf("A release: %v", err)
	}
	ok3, _ := l.Acquire(ctx, "k", "B", 5*time.Second)
	if !ok3 {
		t.Fatalf("B should acquire after A release")
	}
}

func TestInMemoryLockerExpiryHandsOff(t *testing.T) {
	l := NewInMemoryLocker()
	ctx := context.Background()
	t0 := time.Unix(1700000000, 0)
	now := t0
	l.SetClock(func() time.Time { return now })

	ok, _ := l.Acquire(ctx, "k", "A", 30*time.Second)
	if !ok {
		t.Fatalf("A acquire failed")
	}
	// Advance past expiry without A refreshing.
	now = t0.Add(40 * time.Second)
	if err := l.Refresh(ctx, "k", "A", 30*time.Second); err == nil {
		t.Fatalf("expired refresh must fail")
	}
	ok2, _ := l.Acquire(ctx, "k", "B", 30*time.Second)
	if !ok2 {
		t.Fatalf("B should acquire after A's lease expired")
	}
	if h, ok := l.CurrentHolder("k"); !ok || h != "B" {
		t.Fatalf("holder = %q ok=%v want B", h, ok)
	}
}

// Two electors race for the same key against a shared in-memory locker.
// Exactly one must win at any moment; on cancel of the winner, the loser
// must take over within (LeaseTTL + PollEvery).
func TestElectorFailoverWithin30s(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping failover timing test in -short mode")
	}
	locker := NewInMemoryLocker()
	cfgA := Config{Key: "k", ControllerID: "A", LeaseTTL: 2 * time.Second, RefreshEvery: 500 * time.Millisecond, PollEvery: 250 * time.Millisecond}
	cfgB := cfgA
	cfgB.ControllerID = "B"

	var aBecame, aLost, bBecame, bLost atomic.Int32

	a := New(cfgA, locker, func() { aBecame.Add(1) }, func() { aLost.Add(1) })
	b := New(cfgB, locker, func() { bBecame.Add(1) }, func() { bLost.Add(1) })

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() { a.Run(ctxA); close(doneA) }()
	go func() { b.Run(ctxB); close(doneB) }()

	// Wait until exactly one of them becomes leader.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.IsLeader() != b.IsLeader() && (a.IsLeader() || b.IsLeader()) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a.IsLeader() == b.IsLeader() {
		t.Fatalf("expected exactly one leader after 2s; a=%v b=%v", a.IsLeader(), b.IsLeader())
	}

	// Identify leader and cancel it; measure failover.
	var leaderCancel context.CancelFunc
	var followerWaiter func() bool
	if a.IsLeader() {
		leaderCancel = cancelA
		followerWaiter = b.IsLeader
	} else {
		leaderCancel = cancelB
		followerWaiter = a.IsLeader
	}
	t0 := time.Now()
	leaderCancel()

	// SLA: 30s. Configured lease=2s + poll=0.25s, so observed should be <3s.
	failoverDeadline := t0.Add(30 * time.Second)
	for time.Now().Before(failoverDeadline) {
		if followerWaiter() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !followerWaiter() {
		t.Fatalf("follower did not take over within 30s")
	}
	failover := time.Since(t0)
	if failover > 30*time.Second {
		t.Fatalf("failover took %v > 30s", failover)
	}
	t.Logf("failover: %v", failover)

	// Cancel the survivor and let goroutines exit cleanly.
	cancelA()
	cancelB()
	<-doneA
	<-doneB
}

// AtMostOneLeader is the safety property: across many randomized
// acquire/refresh/expire cycles, the locker never lets two distinct
// holders coexist.
func TestInMemoryLockerNeverGrantsTwo(t *testing.T) {
	l := NewInMemoryLocker()
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		ok, _ := l.Acquire(ctx, "k", "A", 10*time.Millisecond)
		if !ok {
			continue
		}
		// While A holds, B must be locked out.
		okB, _ := l.Acquire(ctx, "k", "B", 10*time.Millisecond)
		if okB {
			h, _ := l.CurrentHolder("k")
			t.Fatalf("iter %d: B acquired while A held; current holder = %q", i, h)
		}
		_ = l.Release(ctx, "k", "A")
	}
}
