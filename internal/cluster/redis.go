package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RedisLocker is a Locker that uses a real Redis instance via a tiny
// hand-rolled RESP client. It supports just the commands we need:
// SET key val NX EX ttl, plus EVAL for the compare-and-delete used by
// Refresh/Release.
//
// We avoid pulling go-redis or redigo into go.mod because (a) the
// surface area we need is tiny, (b) it keeps the dep graph small for
// downstream consumers, and (c) the resp serializer is ~30 lines.
//
// The connection is held under a mutex; concurrent calls serialize.
// For the leader-election workload this is fine -- we issue one command
// every RefreshEvery (10s leader) or PollEvery (5s follower).
type RedisLocker struct {
	addr   string
	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
}

// NewRedisLocker constructs a Locker against `addr` (e.g. "127.0.0.1:6379").
// The connection is established lazily on the first command.
func NewRedisLocker(addr string) *RedisLocker {
	return &RedisLocker{addr: addr}
}

// Close terminates the underlying connection. Idempotent.
func (r *RedisLocker) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		err := r.conn.Close()
		r.conn = nil
		r.reader = nil
		return err
	}
	return nil
}

// Acquire issues `SET key holder NX EX ttl_seconds`.
func (r *RedisLocker) Acquire(ctx context.Context, key, holder string, ttl time.Duration) (bool, error) {
	secs := int(ttl.Seconds())
	if secs < 1 {
		secs = 1
	}
	resp, err := r.do(ctx, "SET", key, holder, "NX", "EX", strconv.Itoa(secs))
	if err != nil {
		return false, err
	}
	// "OK" on success, nil bulk on already-held.
	switch v := resp.(type) {
	case string:
		return v == "OK", nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected SET response: %T", resp)
	}
}

// refreshScript is a short Lua snippet that extends the TTL only if the
// caller is still the holder. Equivalent to a CAS extend.
const refreshScript = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('EXPIRE', KEYS[1], ARGV[2]) else return 0 end`

// Refresh extends the TTL if and only if `holder` still owns the key.
func (r *RedisLocker) Refresh(ctx context.Context, key, holder string, ttl time.Duration) error {
	secs := int(ttl.Seconds())
	if secs < 1 {
		secs = 1
	}
	resp, err := r.do(ctx, "EVAL", refreshScript, "1", key, holder, strconv.Itoa(secs))
	if err != nil {
		return err
	}
	if n, ok := resp.(int64); ok && n == 1 {
		return nil
	}
	return ErrNotHeld
}

// releaseScript deletes the key only if the caller is still the holder.
const releaseScript = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`

// Release surrenders the lease iff `holder` still owns it.
func (r *RedisLocker) Release(ctx context.Context, key, holder string) error {
	resp, err := r.do(ctx, "EVAL", releaseScript, "1", key, holder)
	if err != nil {
		return err
	}
	if n, ok := resp.(int64); ok && n == 1 {
		return nil
	}
	return ErrNotHeld
}

// --- minimal RESP client ---

func (r *RedisLocker) ensureConn(ctx context.Context) error {
	if r.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return fmt.Errorf("dial redis %s: %w", r.addr, err)
	}
	r.conn = c
	r.reader = bufio.NewReader(c)
	return nil
}

func (r *RedisLocker) do(ctx context.Context, parts ...string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureConn(ctx); err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = r.conn.SetDeadline(dl)
	} else {
		_ = r.conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := r.conn.Write([]byte(encodeRESP(parts))); err != nil {
		_ = r.conn.Close()
		r.conn = nil
		r.reader = nil
		return nil, err
	}
	return readRESP(r.reader)
}

func encodeRESP(parts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return b.String()
}

func readRESP(r *bufio.Reader) (any, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	switch prefix {
	case '+':
		return line, nil
	case '-':
		return nil, errors.New("redis: " + line)
	case ':':
		return strconv.ParseInt(line, 10, 64)
	case '$':
		n, perr := strconv.Atoi(line)
		if perr != nil {
			return nil, perr
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2) // payload + \r\n
		if _, rerr := io.ReadFull(r, buf); rerr != nil {
			return nil, rerr
		}
		return string(buf[:n]), nil
	case '*':
		n, perr := strconv.Atoi(line)
		if perr != nil {
			return nil, perr
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, n)
		for i := 0; i < n; i++ {
			v, verr := readRESP(r)
			if verr != nil {
				return nil, verr
			}
			out[i] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown RESP prefix %q", prefix)
}

func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	s = strings.TrimRight(s, "\r\n")
	return s, nil
}
