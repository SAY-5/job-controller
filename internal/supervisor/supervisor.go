// Package supervisor owns the per-job goroutine that streams worker stdout,
// parses checkpoint events, and drives state transitions in the store.
package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SAY-5/job-controller/internal/config"
	"github.com/SAY-5/job-controller/internal/docker"
	"github.com/SAY-5/job-controller/internal/store"
)

// LeaderCheck returns true when this controller is the cluster leader and
// is allowed to write new state (start workers, transition jobs). When
// nil, the supervisor behaves as if it is always leader, which is the
// single-controller default.
type LeaderCheck func() bool

// ErrNotLeader is returned by Start when the supervisor is in follower mode.
var ErrNotLeader = errors.New("supervisor: not leader")

// Supervisor manages active jobs.
type Supervisor struct {
	cfg    config.Config
	st     *store.Store
	docker *docker.Client
	pid    int

	leaderCheck  LeaderCheck
	controllerID string

	mu      sync.Mutex
	tracked map[string]*tracker
	wg      sync.WaitGroup
}

type tracker struct {
	jobID       string
	containerID string
	cancel      context.CancelFunc
	done        chan struct{}
}

// New creates a Supervisor. dockerClient may be nil when JOBCTL_NO_DOCKER is set.
func New(cfg config.Config, st *store.Store, dockerClient *docker.Client) *Supervisor {
	return &Supervisor{
		cfg:     cfg,
		st:      st,
		docker:  dockerClient,
		pid:     os.Getpid(),
		tracked: map[string]*tracker{},
	}
}

// SetLeaderCheck installs a leader-gating callback. When the callback
// returns false, Start refuses with ErrNotLeader -- preventing the
// follower controller from racing the leader and spawning duplicate
// worker containers for the same job.
func (s *Supervisor) SetLeaderCheck(controllerID string, check LeaderCheck) {
	s.controllerID = controllerID
	s.leaderCheck = check
}

// ControllerID returns the cluster identity assigned via SetLeaderCheck,
// or the empty string for single-node deployments.
func (s *Supervisor) ControllerID() string { return s.controllerID }

// PID returns the controller PID. Exposed so labels match.
func (s *Supervisor) PID() int { return s.pid }

// Start launches a worker container for the job and begins streaming.
// The job must be in `pending` (fresh launch) or `interrupted_resumable`
// (resume). On success the job moves to `running`.
//
// In HA mode (SetLeaderCheck called), only the leader is allowed to
// start. Followers receive ErrNotLeader; the API surfaces this so callers
// can retry against the current leader. Additionally the assigned
// controller id is recorded on the job row so reaper logic can detect
// stale assignments after a failover.
func (s *Supervisor) Start(ctx context.Context, jobID string) error {
	if s.leaderCheck != nil && !s.leaderCheck() {
		return ErrNotLeader
	}
	job, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.State != store.StatePending && job.State != store.StateInterruptedResumable {
		return fmt.Errorf("supervisor.Start: job %s in state %s", jobID, job.State)
	}

	hostStateDir := filepath.Join(s.cfg.HostStateDir, jobID)
	if err := os.MkdirAll(hostStateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	stateMountTarget := filepath.Dir(s.cfg.WorkerStatePath)
	if stateMountTarget == "" {
		stateMountTarget = "/state"
	}
	containerStatePath := s.cfg.WorkerStatePath
	hostStateFile := filepath.Join(hostStateDir, filepath.Base(containerStatePath))

	cmd := []string{
		"--job-id", jobID,
		"--limit", argOr(job.Args, "--limit", "100000"),
		"--checkpoint-every", argOr(job.Args, "--checkpoint-every", "5000"),
		"--output-state", containerStatePath,
	}
	if v := argOr(job.Args, "--sleep-per-checkpoint-ms", ""); v != "" {
		cmd = append(cmd, "--sleep-per-checkpoint-ms", v)
	}
	// On resume, point the worker at the persisted state file.
	if job.State == store.StateInterruptedResumable && job.LastCheckpointPath != nil {
		cmd = append(cmd, "--resume-from", containerStatePath)
	}

	opts := docker.RunOptions{
		Image:       job.Image,
		Cmd:         cmd,
		CPUs:        "1.0",
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   64,
		Labels: map[string]string{
			docker.LabelJobID:         jobID,
			docker.LabelControllerPID: strconv.Itoa(s.pid),
			docker.LabelCreatedAt:     strconv.FormatInt(time.Now().UnixNano(), 10),
		},
		BindMounts: []docker.BindMount{
			{Host: hostStateDir, Target: stateMountTarget, RW: true},
		},
	}

	containerID, err := s.docker.Run(ctx, opts)
	if err != nil {
		return fmt.Errorf("docker run: %w", err)
	}

	if err := s.st.Transition(ctx, jobID, store.StateRunning, func(u map[string]any) {
		u["container_id"] = containerID
		u["controller_pid"] = s.pid
		u["state_volume_path"] = hostStateFile
		if s.controllerID != "" {
			u["assigned_controller_id"] = s.controllerID
		}
	}); err != nil {
		_ = s.docker.Remove(context.Background(), containerID)
		return err
	}

	s.track(ctx, jobID, containerID)
	return nil
}

// AdoptRunning re-attaches to a worker container that survived a controller
// crash. The job row is left in its existing state; the supervisor merely
// resumes log streaming.
func (s *Supervisor) AdoptRunning(ctx context.Context, jobID, containerID string) {
	s.track(ctx, jobID, containerID)
}

func (s *Supervisor) track(parent context.Context, jobID, containerID string) {
	ctx, cancel := context.WithCancel(parent)
	t := &tracker{
		jobID:       jobID,
		containerID: containerID,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	s.mu.Lock()
	s.tracked[jobID] = t
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(t.done)
		s.runStream(ctx, jobID, containerID)
		s.mu.Lock()
		delete(s.tracked, jobID)
		s.mu.Unlock()
	}()
}

func (s *Supervisor) runStream(ctx context.Context, jobID, containerID string) {
	logs, err := s.docker.FollowLogs(ctx, containerID)
	if err != nil {
		log.Printf("supervisor: follow logs for %s: %v", jobID, err)
		s.markFailed(jobID, "log stream attach failed")
		return
	}
	defer logs.Close()

	scanner := bufio.NewScanner(logs)
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	sawCompleted := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "checkpoint":
			var ev CheckpointEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			if err := s.st.RecordCheckpoint(ctx, jobID, ev.Epoch, ev.Found, ev.StatePath); err != nil {
				log.Printf("supervisor: record checkpoint %s: %v", jobID, err)
			}
		case "completed":
			var ev CompletedEvent
			if err := json.Unmarshal([]byte(line), &ev); err == nil {
				_ = ev
			}
			sawCompleted = true
		case "started":
			// Informational only; no state change beyond what Start() did.
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("supervisor: log scanner for %s: %v", jobID, err)
	}

	// The log stream closed; inspect the container to learn the exit code.
	insp, ierr := s.docker.Inspect(context.Background(), containerID)
	if ierr != nil {
		log.Printf("supervisor: inspect %s after log close: %v", jobID, ierr)
		s.markFailed(jobID, "inspect after exit failed")
		return
	}
	// If the container is still running but our log stream died, the
	// controller is shutting down. Don't transition the job; let the
	// recovery scan handle it on next startup.
	if insp.Running {
		return
	}
	if insp.ExitCode == 0 || sawCompleted {
		_ = s.st.Transition(context.Background(), jobID, store.StateCompleted, func(u map[string]any) {
			u["exit_code"] = insp.ExitCode
		})
	} else {
		_ = s.st.Transition(context.Background(), jobID, store.StateFailed, func(u map[string]any) {
			u["exit_code"] = insp.ExitCode
		})
	}
	// Best-effort cleanup so the next reaper run doesn't see the container.
	_ = s.docker.Remove(context.Background(), containerID)
}

func (s *Supervisor) markFailed(jobID, _ string) {
	_ = s.st.Transition(context.Background(), jobID, store.StateFailed)
}

// Cancel sends SIGTERM to the worker and transitions the job to cancelled
// after the grace period.
func (s *Supervisor) Cancel(ctx context.Context, jobID string) error {
	job, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.ContainerID == nil {
		return s.st.Transition(ctx, jobID, store.StateCancelled)
	}
	if err := s.docker.Stop(ctx, *job.ContainerID, s.cfg.GracePeriod); err != nil {
		log.Printf("supervisor: stop %s: %v", jobID, err)
	}
	return s.st.Transition(ctx, jobID, store.StateCancelled)
}

// ShutdownGraceful broadcasts SIGTERM to every tracked worker and waits up to
// the configured grace period for them to exit. Survivors get SIGKILL.
// Jobs that did not complete cleanly are marked interrupted_resumable when a
// checkpoint exists, otherwise interrupted_unresumable.
func (s *Supervisor) ShutdownGraceful(ctx context.Context, simulatedFault bool) {
	s.mu.Lock()
	trackers := make([]*tracker, 0, len(s.tracked))
	for _, t := range s.tracked {
		trackers = append(trackers, t)
	}
	s.mu.Unlock()

	for _, t := range trackers {
		_ = s.docker.Stop(ctx, t.containerID, s.cfg.GracePeriod)
	}

	deadline := time.Now().Add(s.cfg.GracePeriod + 5*time.Second)
	for _, t := range trackers {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Second
		}
		select {
		case <-t.done:
		case <-time.After(remaining):
			_ = s.docker.Kill(ctx, t.containerID)
		}
	}
	s.wg.Wait()

	for _, t := range trackers {
		s.markInterrupted(ctx, t.jobID, simulatedFault)
	}
}

func (s *Supervisor) markInterrupted(ctx context.Context, jobID string, simulatedFault bool) {
	job, err := s.st.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	if job.State.IsTerminal() {
		return
	}
	target := store.StateInterruptedResumable
	if job.LastCheckpointAt == nil {
		target = store.StateInterruptedUnresumable
	}
	// In a simulated fault, only checkpoints from before the fault are usable.
	// We simulate "after-fault checkpoint loss" by demoting freshly-started
	// jobs (no checkpoint yet) to unresumable. The contract documents this.
	if simulatedFault && job.LastCheckpointAt == nil {
		target = store.StateInterruptedUnresumable
	}
	_ = s.st.Transition(ctx, jobID, target)
}

// Wait blocks until all tracked jobs have finished streaming.
func (s *Supervisor) Wait() {
	s.wg.Wait()
}

// argOr searches command args for "--key" and returns the next token,
// falling back to def. Used to pick up worker-specific flags from job args.
func argOr(args []string, key, def string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key {
			return args[i+1]
		}
	}
	return def
}
