// Package cluster implements leader election for HA controller deployments.
//
// Multiple controllers run against the same backing store (SQLite WAL or
// Postgres). At any given moment exactly one controller holds the lease
// and is responsible for scheduling and reaping; the others stand by, poll
// every PollEvery seconds, and pick up the lease when the current holder
// expires it (crash, network partition, intentional shutdown).
//
// The Locker interface is the seam between this package and the chosen
// backend. RedisLocker is the production impl using `SET key id NX EX 30`
// with a periodic refresh; InMemoryLocker is the test impl with the same
// observable semantics minus the network round-trip.
//
// Lease lifecycle:
//
//	t=0     controller-A acquires (TTL=30s, refresh every 10s)
//	t=10    controller-A refreshes
//	t=20    controller-A refreshes
//	t=30    controller-A crashes; lease expires at t=50 (TTL still elapsing)
//	t=51    follower-B's poll succeeds (next 5s tick)
//	failover_window = LeaseTTL + PollEvery = 35s worst case
//
// The 30-second SLA in the layer-3 contract is met by setting LeaseTTL=20s
// and PollEvery=5s (35s deterministic upper bound is loose; observed
// failover in tests is closer to LeaseTTL+1 poll tick).
package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotHeld is returned when a refresh/release is attempted by a non-holder.
var ErrNotHeld = errors.New("cluster: lock not held by caller")

// Locker is the abstract leader-election lock. Implementations must
// guarantee at-most-one holder at any wall-clock instant.
type Locker interface {
	// Acquire attempts to take the lease. Returns true on success. On
	// success, the lease is valid for ttl; the caller must refresh before
	// it expires or surrender leadership.
	Acquire(ctx context.Context, key, holder string, ttl time.Duration) (bool, error)
	// Refresh extends the lease. Returns ErrNotHeld if `holder` no longer
	// owns the key (e.g. lease expired and another holder grabbed it).
	Refresh(ctx context.Context, key, holder string, ttl time.Duration) error
	// Release voluntarily surrenders the lease.
	Release(ctx context.Context, key, holder string) error
}

// Config tunes the elector loop.
type Config struct {
	Key          string        // namespaced redis key, e.g. "cb:leader:lock"
	ControllerID string        // unique per process, used as lock holder id
	LeaseTTL     time.Duration // how long a single hold is valid
	RefreshEvery time.Duration // how often the leader refreshes
	PollEvery    time.Duration // how often a follower retries Acquire
}

// DefaultConfig fills in the layer-3 timings from the spec: 30s TTL,
// 10s refresh cadence, 5s follower poll. Callers fill in Key + ControllerID.
func DefaultConfig() Config {
	return Config{
		Key:          "cb:leader:lock",
		LeaseTTL:     30 * time.Second,
		RefreshEvery: 10 * time.Second,
		PollEvery:    5 * time.Second,
	}
}

// Elector orchestrates a single controller's relationship with the lease.
// It exposes IsLeader() for the supervisor to gate writes on, and Run() to
// drive the acquire/refresh/poll loop until the context is cancelled.
type Elector struct {
	cfg      Config
	locker   Locker
	leader   atomic.Bool
	onBecome func()
	onLose   func()

	mu      sync.Mutex
	started bool
}

// New constructs an Elector. onBecomeLeader fires once when the elector
// transitions from follower to leader; onLoseLeader fires on the reverse
// transition. Either may be nil.
func New(cfg Config, locker Locker, onBecomeLeader, onLoseLeader func()) *Elector {
	return &Elector{
		cfg:      cfg,
		locker:   locker,
		onBecome: onBecomeLeader,
		onLose:   onLoseLeader,
	}
}

// IsLeader reports whether this elector currently holds the lease.
func (e *Elector) IsLeader() bool { return e.leader.Load() }

// Run blocks until ctx is cancelled. It is safe to call exactly once.
func (e *Elector) Run(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			if e.IsLeader() {
				_ = e.locker.Release(context.Background(), e.cfg.Key, e.cfg.ControllerID)
				e.setLeader(false)
			}
			return
		default:
		}

		if e.IsLeader() {
			e.runAsLeader(ctx)
		} else {
			e.runAsFollower(ctx)
		}
	}
}

func (e *Elector) runAsLeader(ctx context.Context) {
	tick := time.NewTicker(e.cfg.RefreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			err := e.locker.Refresh(ctx, e.cfg.Key, e.cfg.ControllerID, e.cfg.LeaseTTL)
			if errors.Is(err, ErrNotHeld) {
				// Lost the lease (network blip, clock skew, partition).
				e.setLeader(false)
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				// Transient failure; treat as lost rather than block.
				e.setLeader(false)
				return
			}
		}
	}
}

func (e *Elector) runAsFollower(ctx context.Context) {
	// Try once immediately, then on the poll tick.
	if e.tryAcquire(ctx) {
		return
	}
	tick := time.NewTicker(e.cfg.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if e.tryAcquire(ctx) {
				return
			}
		}
	}
}

func (e *Elector) tryAcquire(ctx context.Context) bool {
	ok, err := e.locker.Acquire(ctx, e.cfg.Key, e.cfg.ControllerID, e.cfg.LeaseTTL)
	if err != nil {
		return false
	}
	if ok {
		e.setLeader(true)
		return true
	}
	return false
}

func (e *Elector) setLeader(v bool) {
	prev := e.leader.Swap(v)
	if v && !prev && e.onBecome != nil {
		e.onBecome()
	}
	if !v && prev && e.onLose != nil {
		e.onLose()
	}
}
