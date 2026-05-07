// Command reaper finds orphaned worker containers (those whose creating
// controller PID is no longer alive on this host) and prints them as JSON
// lines. With --remove the orphans are force-removed. The tool is intended
// for ad-hoc cleanup; the controller does the same work on startup via the
// recovery package.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/SAY-5/job-controller/internal/docker"
)

func main() {
	remove := flag.Bool("remove", false, "remove orphaned containers after listing")
	flag.Parse()

	dc, err := docker.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reaper: ", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	containers, err := dc.ListByLabel(ctx, docker.LabelJobID, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reaper: list:", err)
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	orphans := 0
	for _, c := range containers {
		pidStr := c.Labels[docker.LabelControllerPID]
		pid, _ := strconv.Atoi(pidStr)
		alive := pidAlive(pid)
		if alive {
			continue
		}
		orphans++
		_ = enc.Encode(map[string]any{
			"id":             c.ID,
			"job_id":         c.Labels[docker.LabelJobID],
			"controller_pid": pid,
			"state":          c.State,
		})
		if *remove {
			rmCtx, rc := context.WithTimeout(context.Background(), 10*time.Second)
			if err := dc.Remove(rmCtx, c.ID); err != nil {
				fmt.Fprintf(os.Stderr, "reaper: remove %s: %v\n", c.ID, err)
			}
			rc()
		}
	}
	fmt.Fprintf(os.Stderr, "reaper: found %d orphan(s)\n", orphans)
}

// pidAlive returns true if a process with the given pid exists on this host.
// Using signal 0 is the standard portable POSIX trick.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
