// Package store wraps SQLite with a state-machine log for jobs. The schema
// keeps one row per job plus an append-only events table; every state
// transition is wrapped in a transaction with an explicit WAL checkpoint at
// boundaries.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// State is the job lifecycle state.
type State string

const (
	StatePending                State = "pending"
	StateRunning                State = "running"
	StateCheckpointing          State = "checkpointing"
	StateCompleted              State = "completed"
	StateFailed                 State = "failed"
	StateCancelled              State = "cancelled"
	StateInterruptedResumable   State = "interrupted_resumable"
	StateInterruptedUnresumable State = "interrupted_unresumable"
)

// IsTerminal returns true when no further transitions are expected.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateInterruptedUnresumable:
		return true
	}
	return false
}

// validTransitions enforces the documented state machine. Self-transitions
// are accepted as no-ops to make recovery idempotent.
var validTransitions = map[State]map[State]bool{
	StatePending: {
		StateRunning:                true,
		StateCancelled:              true,
		StateFailed:                 true,
		StateInterruptedUnresumable: true,
	},
	StateRunning: {
		StateRunning:                true,
		StateCheckpointing:          true,
		StateCompleted:              true,
		StateFailed:                 true,
		StateCancelled:              true,
		StateInterruptedResumable:   true,
		StateInterruptedUnresumable: true,
	},
	StateCheckpointing: {
		StateRunning:                true,
		StateCheckpointing:          true,
		StateCompleted:              true,
		StateFailed:                 true,
		StateCancelled:              true,
		StateInterruptedResumable:   true,
		StateInterruptedUnresumable: true,
	},
	StateInterruptedResumable: {
		StateRunning:                true,
		StateInterruptedUnresumable: true,
		StateCancelled:              true,
	},
	// Terminal states allow only self-transitions (idempotent no-op).
	StateCompleted:              {StateCompleted: true},
	StateFailed:                 {StateFailed: true},
	StateCancelled:              {StateCancelled: true},
	StateInterruptedUnresumable: {StateInterruptedUnresumable: true},
}

// IsValidTransition reports whether moving from -> to is allowed.
func IsValidTransition(from, to State) bool {
	if from == to {
		return true
	}
	m, ok := validTransitions[from]
	if !ok {
		return false
	}
	return m[to]
}

var (
	ErrNotFound          = errors.New("job not found")
	ErrInvalidTransition = errors.New("invalid state transition")
)

// Job is the row representation of a managed job.
type Job struct {
	ID                   string
	Image                string
	Command              string
	Args                 []string
	State                State
	CreatedAt            time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	ExitCode             *int
	LastCheckpointAt     *time.Time
	LastCheckpointPath   *string
	LastCheckpointEpoch  *int64
	LastCheckpointFound  *int64
	ContainerID          *string
	ControllerPID        *int
	StateVolumePath      *string
	AssignedControllerID *string
}

// Event is the row representation of a job event.
type Event struct {
	ID         int64
	JobID      string
	EventType  string
	Payload    json.RawMessage
	RecordedAt time.Time
}

// Store wraps a *sql.DB with the job operations. It is safe for concurrent use.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// Open opens (or creates) the SQLite database at path with WAL mode enabled.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := ensureDir(path); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_journal=WAL&_timeout=5000&_synchronous=NORMAL&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single connection avoids cross-thread WAL surprises with go-sqlite3.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Additive migration: pre-v3 databases were created without the
	// assigned_controller_id column. ALTER fails idempotently with a
	// "duplicate column" error which we treat as success.
	if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN assigned_controller_id TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate assigned_controller_id: %w", err)
		}
	}
	return &Store{db: db, clock: time.Now}, nil
}

// Close closes the underlying database. Performs a FULL WAL checkpoint first.
func (s *Store) Close() error {
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(FULL)")
	return s.db.Close()
}

// CreateJob inserts a fresh job in `pending`.
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	if j.State == "" {
		j.State = StatePending
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = s.clock()
	}
	argsJSON, err := json.Marshal(j.Args)
	if err != nil {
		return fmt.Errorf("marshal args: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (id, image, command, args_json, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, j.ID, j.Image, j.Command, string(argsJSON), string(j.State), j.CreatedAt.UnixNano()); err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, j.ID, "created", map[string]any{"state": j.State}, s.clock()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	return nil
}

// GetJob loads a single job.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, image, command, args_json, state, created_at, started_at, finished_at,
		       exit_code, last_checkpoint_at, last_checkpoint_path, last_checkpoint_epoch,
		       last_checkpoint_found, container_id, controller_pid, state_volume_path,
		       assigned_controller_id
		FROM jobs WHERE id = ?
	`, id)
	return scanJob(row)
}

// ListJobs returns jobs in created-at descending order. afterID is a cursor
// (set to "" for the first page); state filters by exact match (empty = all).
func (s *Store) ListJobs(ctx context.Context, state State, afterID string, limit int) ([]Job, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, image, command, args_json, state, created_at, started_at, finished_at,
	             exit_code, last_checkpoint_at, last_checkpoint_path, last_checkpoint_epoch,
	             last_checkpoint_found, container_id, controller_pid, state_volume_path,
	             assigned_controller_id
	      FROM jobs`
	clauses := []string{}
	args := []any{}
	if state != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, string(state))
	}
	if afterID != "" {
		// cursor by created_at descending; we encode the cursor as job id and
		// use the id's created_at as the seek key.
		clauses = append(clauses, `created_at < (SELECT created_at FROM jobs WHERE id = ?)`)
		args = append(args, afterID)
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if len(out) > limit {
		nextCursor = out[limit-1].ID
		out = out[:limit]
	}
	return out, nextCursor, nil
}

// Transition moves a job from its current state to next. Setters lets the
// caller modify additional columns within the same transaction (e.g. setting
// container_id when transitioning to running). Self-transitions are no-ops
// but still write an event row for audit.
func (s *Store) Transition(ctx context.Context, id string, next State, setters ...func(map[string]any)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	var current string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	cur := State(current)
	if !IsValidTransition(cur, next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cur, next)
	}

	updates := map[string]any{
		"state": string(next),
	}
	for _, f := range setters {
		f(updates)
	}
	switch next {
	case StateRunning:
		if _, set := updates["started_at"]; !set {
			updates["started_at"] = s.clock().UnixNano()
		}
	case StateCompleted, StateFailed, StateCancelled, StateInterruptedUnresumable:
		if _, set := updates["finished_at"]; !set {
			updates["finished_at"] = s.clock().UnixNano()
		}
	}

	cols := make([]string, 0, len(updates))
	args := make([]any, 0, len(updates)+1)
	for k, v := range updates {
		cols = append(cols, k+" = ?")
		args = append(args, v)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET "+strings.Join(cols, ", ")+" WHERE id = ?", args...); err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, id, "transition", map[string]any{
		"from": string(cur), "to": string(next), "updates": redactedUpdates(updates),
	}, s.clock()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Boundary checkpoints: full at terminal, passive otherwise.
	mode := "PASSIVE"
	if next.IsTerminal() {
		mode = "FULL"
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(" + mode + ")"); err == nil {
		_, _ = s.db.Exec("UPDATE wal_marker SET last_checkpoint = ? WHERE id = 1", s.clock().UnixNano())
	}
	return nil
}

// RecordCheckpoint persists the latest checkpoint metadata. It does not
// transition the state by itself; callers transition through checkpointing
// when they need a clean audit record.
func (s *Store) RecordCheckpoint(ctx context.Context, id string, epoch, found int64, path string) error {
	now := s.clock().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE jobs SET last_checkpoint_at = ?, last_checkpoint_path = ?,
		                last_checkpoint_epoch = ?, last_checkpoint_found = ?
		WHERE id = ?
	`, now, path, epoch, found, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := insertEvent(ctx, tx, id, "checkpoint", map[string]any{
		"epoch": epoch, "found": found, "path": path,
	}, s.clock()); err != nil {
		return err
	}
	return tx.Commit()
}

// LastWALCheckpoint returns the last recorded WAL checkpoint timestamp, or
// the zero time if none has been recorded.
func (s *Store) LastWALCheckpoint(ctx context.Context) (time.Time, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT last_checkpoint FROM wal_marker WHERE id = 1`).Scan(&v)
	if err != nil {
		return time.Time{}, err
	}
	if v == 0 {
		return time.Time{}, nil
	}
	return time.Unix(0, v), nil
}

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// RecentEvents returns the last `limit` events for a job, newest first.
func (s *Store) RecentEvents(ctx context.Context, jobID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, event_type, payload, recorded_at FROM job_events
		WHERE job_id = ? ORDER BY id DESC LIMIT ?
	`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		var payload string
		if err := rows.Scan(&e.ID, &e.JobID, &e.EventType, &payload, &ts); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		e.RecordedAt = time.Unix(0, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobsInStates returns jobs whose state matches any of the supplied states.
// Used by the recovery scan on startup.
func (s *Store) JobsInStates(ctx context.Context, states ...State) ([]Job, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	q := `SELECT id, image, command, args_json, state, created_at, started_at, finished_at,
	             exit_code, last_checkpoint_at, last_checkpoint_path, last_checkpoint_epoch,
	             last_checkpoint_found, container_id, controller_pid, state_volume_path,
	             assigned_controller_id
	      FROM jobs WHERE state IN (` + strings.Join(placeholders, ",") + `)
	      ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// --- helpers ---

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(rs rowScanner) (*Job, error) {
	var j Job
	var argsJSON string
	var state string
	var createdAt int64
	var startedAt, finishedAt, ckptAt sql.NullInt64
	var exitCode sql.NullInt64
	var ckptPath, containerID, volumePath, command, assignedCtl sql.NullString
	var ckptEpoch, ckptFound sql.NullInt64
	var controllerPID sql.NullInt64

	err := rs.Scan(&j.ID, &j.Image, &command, &argsJSON, &state, &createdAt,
		&startedAt, &finishedAt, &exitCode, &ckptAt, &ckptPath, &ckptEpoch,
		&ckptFound, &containerID, &controllerPID, &volumePath, &assignedCtl)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	j.Command = command.String
	j.State = State(state)
	j.CreatedAt = time.Unix(0, createdAt)
	if startedAt.Valid {
		t := time.Unix(0, startedAt.Int64)
		j.StartedAt = &t
	}
	if finishedAt.Valid {
		t := time.Unix(0, finishedAt.Int64)
		j.FinishedAt = &t
	}
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		j.ExitCode = &ec
	}
	if ckptAt.Valid {
		t := time.Unix(0, ckptAt.Int64)
		j.LastCheckpointAt = &t
	}
	if ckptPath.Valid {
		s := ckptPath.String
		j.LastCheckpointPath = &s
	}
	if ckptEpoch.Valid {
		v := ckptEpoch.Int64
		j.LastCheckpointEpoch = &v
	}
	if ckptFound.Valid {
		v := ckptFound.Int64
		j.LastCheckpointFound = &v
	}
	if containerID.Valid {
		s := containerID.String
		j.ContainerID = &s
	}
	if controllerPID.Valid {
		v := int(controllerPID.Int64)
		j.ControllerPID = &v
	}
	if volumePath.Valid {
		s := volumePath.String
		j.StateVolumePath = &s
	}
	if assignedCtl.Valid {
		s := assignedCtl.String
		j.AssignedControllerID = &s
	}
	if err := json.Unmarshal([]byte(argsJSON), &j.Args); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	return &j, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, jobID, eventType string, payload any, ts time.Time) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO job_events (job_id, event_type, payload, recorded_at)
		VALUES (?, ?, ?, ?)
	`, jobID, eventType, string(b), ts.UnixNano())
	return err
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func redactedUpdates(u map[string]any) map[string]any {
	// Avoid logging large payloads in event audit.
	out := make(map[string]any, len(u))
	for k, v := range u {
		switch k {
		case "state", "exit_code", "container_id", "last_checkpoint_epoch",
			"last_checkpoint_found", "started_at", "finished_at",
			"assigned_controller_id":
			out[k] = v
		}
	}
	return out
}

func ensureDir(p string) error {
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return nil
	}
	return mkdirAll(dir)
}
