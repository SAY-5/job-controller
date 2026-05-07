// Package api implements the REST surface. It is intentionally small: create,
// list, get, cancel, and a healthz probe that proves SQLite + Docker are
// reachable and reports the last WAL checkpoint timestamp.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SAY-5/job-controller/internal/config"
	"github.com/SAY-5/job-controller/internal/docker"
	"github.com/SAY-5/job-controller/internal/store"
	"github.com/SAY-5/job-controller/internal/supervisor"

	"github.com/google/uuid"
)

// Server holds dependencies. Constructed from main.
type Server struct {
	cfg        config.Config
	store      *store.Store
	docker     *docker.Client
	supervisor *supervisor.Supervisor
}

func NewServer(cfg config.Config, st *store.Store, dc *docker.Client, sup *supervisor.Supervisor) *Server {
	return &Server{cfg: cfg, store: st, docker: dc, supervisor: sup}
}

// Handler returns the wired-up http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/jobs", s.handleJobs)
	mux.HandleFunc("/v1/jobs/", s.handleJobByID)
	return requestIDMiddleware(mux)
}

// --- types ---

type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id"`
}

type createJobRequest struct {
	Image                string   `json:"image"`
	Command              string   `json:"command"`
	Args                 []string `json:"args"`
	CheckpointEvery      int      `json:"checkpoint_every"`
	Limit                int64    `json:"limit"`
	SleepPerCheckpointMs int      `json:"sleep_per_checkpoint_ms"`
}

type jobView struct {
	ID                  string     `json:"id"`
	Image               string     `json:"image"`
	State               string     `json:"state"`
	CreatedAt           time.Time  `json:"created_at"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	ExitCode            *int       `json:"exit_code,omitempty"`
	LastCheckpointAt    *time.Time `json:"last_checkpoint_at,omitempty"`
	LastCheckpointEpoch *int64     `json:"last_checkpoint_epoch,omitempty"`
	LastCheckpointFound *int64     `json:"last_checkpoint_found,omitempty"`
	ContainerID         *string    `json:"container_id,omitempty"`
	StateVolumePath     *string    `json:"state_volume_path,omitempty"`
}

type listJobsResponse struct {
	Jobs       []jobView `json:"jobs"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type jobDetailResponse struct {
	Job    jobView     `json:"job"`
	Events []eventView `json:"events"`
}

type eventView struct {
	ID         int64           `json:"id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
	RecordedAt time.Time       `json:"recorded_at"`
}

// --- handlers ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	type health struct {
		SQLite        bool      `json:"sqlite"`
		Docker        bool      `json:"docker"`
		LastWAL       time.Time `json:"last_wal_checkpoint"`
		ControllerPID int       `json:"controller_pid"`
	}
	h := health{ControllerPID: s.supervisor.PID()}
	if err := s.store.Ping(r.Context()); err == nil {
		h.SQLite = true
	}
	if s.docker != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.docker.Ping(ctx); err == nil {
			h.Docker = true
		}
	}
	if t, err := s.store.LastWALCheckpoint(r.Context()); err == nil {
		h.LastWAL = t
	}
	status := http.StatusOK
	if !h.SQLite {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createJob(w, r)
	case http.MethodGet:
		s.listJobs(w, r)
	default:
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or POST", false)
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, http.ErrBodyReadAfterClose) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body", false)
		return
	}

	image := req.Image
	if image == "" {
		image = s.cfg.DefaultImage
	}
	args := req.Args
	if req.Limit > 0 {
		args = append(args, "--limit", strconv.FormatInt(req.Limit, 10))
	}
	if req.CheckpointEvery > 0 {
		args = append(args, "--checkpoint-every", strconv.Itoa(req.CheckpointEvery))
	}
	if req.SleepPerCheckpointMs > 0 {
		args = append(args, "--sleep-per-checkpoint-ms", strconv.Itoa(req.SleepPerCheckpointMs))
	}
	command := req.Command
	if command == "" {
		command = "/usr/local/bin/jobworker"
	}

	id := uuid.NewString()
	job := store.Job{
		ID:      id,
		Image:   image,
		Command: command,
		Args:    args,
	}
	ctx := r.Context()
	if err := s.store.CreateJob(ctx, job); err != nil {
		writeError(w, r, http.StatusInternalServerError, "db_error", err.Error(), true)
		return
	}
	go func() {
		bg := context.Background()
		if err := s.supervisor.Start(bg, id); err != nil {
			// Failure to launch flips the job to failed for visibility.
			_ = s.store.Transition(bg, id, store.StateFailed, func(u map[string]any) {
				u["exit_code"] = -1
			})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": store.StatePending})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateFilter := store.State(q.Get("state"))
	cursor := q.Get("cursor")
	limit, _ := strconv.Atoi(q.Get("limit"))
	jobs, next, err := s.store.ListJobs(r.Context(), stateFilter, cursor, limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "db_error", err.Error(), true)
		return
	}
	views := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, toView(&j))
	}
	writeJSON(w, http.StatusOK, listJobsResponse{Jobs: views, NextCursor: next})
}

func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "missing job id", false)
		return
	}
	if len(parts) == 1 {
		s.getJob(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		s.cancelJob(w, r, id)
		return
	}
	writeError(w, r, http.StatusNotFound, "not_found", "no such route", false)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", false)
		return
	}
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "no such job", false)
			return
		}
		writeError(w, r, http.StatusInternalServerError, "db_error", err.Error(), true)
		return
	}
	events, err := s.store.RecentEvents(r.Context(), id, 25)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "db_error", err.Error(), true)
		return
	}
	evs := make([]eventView, 0, len(events))
	for _, e := range events {
		evs = append(evs, eventView{
			ID:         e.ID,
			EventType:  e.EventType,
			Payload:    e.Payload,
			RecordedAt: e.RecordedAt,
		})
	}
	writeJSON(w, http.StatusOK, jobDetailResponse{Job: toView(j), Events: evs})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if err := s.supervisor.Cancel(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "no such job", false)
			return
		}
		writeError(w, r, http.StatusInternalServerError, "cancel_failed", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": store.StateCancelled})
}

// --- helpers ---

func toView(j *store.Job) jobView {
	return jobView{
		ID:                  j.ID,
		Image:               j.Image,
		State:               string(j.State),
		CreatedAt:           j.CreatedAt,
		StartedAt:           j.StartedAt,
		FinishedAt:          j.FinishedAt,
		ExitCode:            j.ExitCode,
		LastCheckpointAt:    j.LastCheckpointAt,
		LastCheckpointEpoch: j.LastCheckpointEpoch,
		LastCheckpointFound: j.LastCheckpointFound,
		ContainerID:         j.ContainerID,
		StateVolumePath:     j.StateVolumePath,
	}
}

type ctxKey int

const ctxKeyRequestID ctxKey = 0

func requestIDFrom(r *http.Request) string {
	if v, _ := r.Context().Value(ctxKeyRequestID).(string); v != "" {
		return v
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, retryable bool) {
	writeJSON(w, status, errorEnvelope{
		Code:      code,
		Message:   msg,
		Retryable: retryable,
		RequestID: requestIDFrom(r),
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			rid = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := contextWithRequestID(r.Context(), rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWithRequestID(ctx context.Context, rid string) context.Context {
	return contextWith(ctx, ctxKeyRequestID, rid)
}

// indirection so go vet doesn't flag us for putting strings in context.
func contextWith(ctx context.Context, k ctxKey, v string) context.Context {
	return context.WithValue(ctx, k, v)
}
