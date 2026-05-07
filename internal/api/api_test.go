package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SAY-5/job-controller/internal/config"
	"github.com/SAY-5/job-controller/internal/store"
	"github.com/SAY-5/job-controller/internal/supervisor"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Config{
		Listen:          ":0",
		DBPath:          ":memory:",
		DefaultImage:    "jobctl/worker:test",
		WorkerStatePath: "/state/state.bin",
		HostStateDir:    dir,
		NoDocker:        true,
	}
	sup := supervisor.New(cfg, st, nil)
	return NewServer(cfg, st, nil, sup), st
}

func TestHealthzReportsSqliteOk(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["sqlite"] != true {
		t.Fatalf("sqlite=false: %v", body)
	}
	// Without a docker client, docker should be reported false.
	if body["docker"] != false {
		t.Fatalf("docker should be false in no-docker mode: %v", body)
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if got := w.Header().Get("X-Request-ID"); got != "abc-123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
}

func TestNotFoundJobReturns404(t *testing.T) {
	s, _ := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs/does-not-exist", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "not_found" {
		t.Fatalf("code = %s", env.Code)
	}
	if env.RequestID == "" {
		t.Fatalf("expected request id in error envelope")
	}
}

func TestListJobsReturnsCreatedJobs(t *testing.T) {
	s, st := newTestServer(t)
	// Seed two jobs directly in the store; we don't go through POST /v1/jobs
	// here because that path tries to launch a container.
	for _, id := range []string{"j-1", "j-2"} {
		if err := st.CreateJob(context.Background(), store.Job{
			ID: id, Image: "img", Command: "/bin/echo",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs?limit=10", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp listJobsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Jobs) != 2 {
		t.Fatalf("got %d jobs", len(resp.Jobs))
	}
	ids := []string{resp.Jobs[0].ID, resp.Jobs[1].ID}
	if !contains(ids, "j-1") || !contains(ids, "j-2") {
		t.Fatalf("missing job ids: %v", ids)
	}
}

func TestGetJobReturnsEvents(t *testing.T) {
	s, st := newTestServer(t)
	if err := st.CreateJob(context.Background(), store.Job{
		ID: "j-1", Image: "img", Command: "/bin/echo",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.RecordCheckpoint(context.Background(), "j-1", 1, 10, "/state/state.bin"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/jobs/j-1", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "checkpoint") {
		t.Fatalf("body missing checkpoint event: %s", w.Body.String())
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
