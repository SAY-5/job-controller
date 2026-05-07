package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client wraps docker CLI invocations. The CLI is preferred over the SDK
// because (a) GitHub Actions runners have it natively, (b) it lets us keep
// the Go module graph tiny, and (c) the operations we need are well covered
// (run, ps, kill, logs, inspect, rm).
type Client struct {
	bin string
	mu  sync.Mutex
}

// NewClient returns a Client that uses the system `docker` binary. If
// JOBCTL_DOCKER_BIN is set, that path is used instead (useful for tests).
func NewClient() (*Client, error) {
	bin := os.Getenv("JOBCTL_DOCKER_BIN")
	if bin == "" {
		bin = "docker"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("docker binary %q not found: %w", bin, err)
	}
	return &Client{bin: bin}, nil
}

// Ping verifies the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.bin, "version", "--format", "{{.Server.Version}}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker ping: %w (%s)", err, stderr.String())
	}
	return nil
}

// RunOptions parameterizes a worker container launch.
type RunOptions struct {
	Image       string
	Name        string
	Cmd         []string
	Labels      map[string]string
	Env         []string
	BindMounts  []BindMount
	CPUs        string // "1.0"
	MemoryBytes int64
	PidsLimit   int64
	NetworkMode string // "" defaults to bridge
	WorkingDir  string
}

// BindMount mirrors the docker -v argument.
type BindMount struct {
	Host   string
	Target string
	RW     bool
}

// Run starts a container and returns its ID. Stdout/stderr stream over
// `docker logs --follow` invoked separately by Attach.
func (c *Client) Run(ctx context.Context, opts RunOptions) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	args := []string{"run", "-d", "--init"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for _, e := range opts.Env {
		args = append(args, "-e", e)
	}
	for _, b := range opts.BindMounts {
		mode := "ro"
		if b.RW {
			mode = "rw"
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s:%s", b.Host, b.Target, mode))
	}
	if opts.CPUs != "" {
		args = append(args, "--cpus", opts.CPUs)
	}
	if opts.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(opts.MemoryBytes, 10))
	}
	if opts.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.FormatInt(opts.PidsLimit, 10))
	}
	if opts.NetworkMode != "" {
		args = append(args, "--network", opts.NetworkMode)
	}
	if opts.WorkingDir != "" {
		args = append(args, "-w", opts.WorkingDir)
	}
	args = append(args, opts.Image)
	args = append(args, opts.Cmd...)

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker run: %w (%s)", err, strings.TrimSpace(errOut.String()))
	}
	id := strings.TrimSpace(out.String())
	if id == "" {
		return "", errors.New("docker run returned empty container id")
	}
	return id, nil
}

// FollowLogs returns an io.ReadCloser streaming the container's combined logs
// from start. The caller is responsible for closing the reader (which will
// terminate the underlying `docker logs --follow` process).
func (c *Client) FollowLogs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, c.bin, "logs", "--follow", "--timestamps=false", containerID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{cmd: cmd, ReadCloser: stdout}, nil
}

type cmdReader struct {
	cmd *exec.Cmd
	io.ReadCloser
}

func (r *cmdReader) Close() error {
	_ = r.ReadCloser.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	_ = r.cmd.Wait()
	return nil
}

// Inspect returns the container's running state and exit code.
type Inspect struct {
	ID       string
	Running  bool
	ExitCode int
	Labels   map[string]string
}

func (c *Client) Inspect(ctx context.Context, containerID string) (*Inspect, error) {
	cmd := exec.CommandContext(ctx, c.bin, "inspect", containerID)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker inspect: %w (%s)", err, errOut.String())
	}
	var raw []struct {
		ID    string `json:"Id"`
		State struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("container %q not found", containerID)
	}
	return &Inspect{
		ID:       raw[0].ID,
		Running:  raw[0].State.Running,
		ExitCode: raw[0].State.ExitCode,
		Labels:   raw[0].Config.Labels,
	}, nil
}

// Stop sends SIGTERM and waits up to grace, then SIGKILL.
func (c *Client) Stop(ctx context.Context, containerID string, grace time.Duration) error {
	secs := int(grace.Seconds())
	if secs < 1 {
		secs = 1
	}
	cmd := exec.CommandContext(ctx, c.bin, "stop", "-t", strconv.Itoa(secs), containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Kill sends SIGKILL immediately.
func (c *Client) Kill(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, c.bin, "kill", containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Remove force-removes a container.
func (c *Client) Remove(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, c.bin, "rm", "-f", containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ListByLabel returns containers (running and exited) carrying labelKey=labelValue.
type ContainerSummary struct {
	ID     string
	Image  string
	State  string
	Labels map[string]string
}

func (c *Client) ListByLabel(ctx context.Context, labelKey, labelValue string) ([]ContainerSummary, error) {
	filter := labelKey
	if labelValue != "" {
		filter = labelKey + "=" + labelValue
	}
	cmd := exec.CommandContext(ctx, c.bin, "ps", "-a",
		"--filter", "label="+filter,
		"--format", "{{.ID}}")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker ps: %w (%s)", err, errOut.String())
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		ids = append(ids, line)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect"}, ids...)
	icmd := exec.CommandContext(ctx, c.bin, args...)
	var iout bytes.Buffer
	icmd.Stdout = &iout
	icmd.Stderr = os.Stderr
	if err := icmd.Run(); err != nil {
		return nil, fmt.Errorf("docker inspect bulk: %w", err)
	}
	var raw []struct {
		ID    string `json:"Id"`
		Image string `json:"Image"`
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(iout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	out2 := make([]ContainerSummary, 0, len(raw))
	for _, r := range raw {
		out2 = append(out2, ContainerSummary{
			ID:     r.ID,
			Image:  r.Image,
			State:  r.State.Status,
			Labels: r.Config.Labels,
		})
	}
	return out2, nil
}
