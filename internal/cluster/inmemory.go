package cluster

import (
	"context"
	"sync"
	"time"
)

// InMemoryLocker is a Locker implementation that uses an in-process mutex
// + map. Used by tests and the integration harness; semantically identical
// to RedisLocker minus durability and cross-process visibility.
//
// All timestamp comparisons use the configured Now func so tests can drive
// the clock manually.
type InMemoryLocker struct {
	mu    sync.Mutex
	holds map[string]inMemoryHold
	now   func() time.Time
}

type inMemoryHold struct {
	holder    string
	expiresAt time.Time
}

// NewInMemoryLocker returns a fresh in-process locker. now defaults to time.Now.
func NewInMemoryLocker() *InMemoryLocker {
	return &InMemoryLocker{
		holds: map[string]inMemoryHold{},
		now:   time.Now,
	}
}

// SetClock overrides the time source. Test-only.
func (l *InMemoryLocker) SetClock(f func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = f
}

// Acquire returns true iff the lease is unheld OR held by `holder` already
// OR expired. Mirrors `SET key holder NX EX ttl` Redis semantics.
func (l *InMemoryLocker) Acquire(_ context.Context, key, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h, ok := l.holds[key]
	if !ok || now.After(h.expiresAt) || h.holder == holder {
		l.holds[key] = inMemoryHold{holder: holder, expiresAt: now.Add(ttl)}
		return true, nil
	}
	return false, nil
}

// Refresh extends the lease iff `holder` still owns it.
func (l *InMemoryLocker) Refresh(_ context.Context, key, holder string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h, ok := l.holds[key]
	if !ok || h.holder != holder || now.After(h.expiresAt) {
		return ErrNotHeld
	}
	l.holds[key] = inMemoryHold{holder: holder, expiresAt: now.Add(ttl)}
	return nil
}

// Release voluntarily surrenders the lease.
func (l *InMemoryLocker) Release(_ context.Context, key, holder string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.holds[key]
	if !ok || h.holder != holder {
		return ErrNotHeld
	}
	delete(l.holds, key)
	return nil
}

// CurrentHolder returns the current holder string and whether the lease is
// active. Test-only helper for assertions.
func (l *InMemoryLocker) CurrentHolder(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.holds[key]
	if !ok || l.now().After(h.expiresAt) {
		return "", false
	}
	return h.holder, true
}
