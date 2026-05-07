// Package recovery implements the startup scan that reconciles the persisted
// job log with live Docker state. The scan handles three crash modes:
//
//  1. Controller died, worker survived: re-attach via labels, continue.
//  2. Both died, checkpoint exists: mark interrupted_resumable, schedule resume.
//  3. Both died, no checkpoint: mark interrupted_unresumable. Work is lost,
//     and only because nothing was ever persisted to begin with.
package recovery

import (
	"context"
	"log"
	"strconv"

	"github.com/SAY-5/job-controller/internal/docker"
	"github.com/SAY-5/job-controller/internal/store"
	"github.com/SAY-5/job-controller/internal/supervisor"
)

type Reconciler struct {
	store      *store.Store
	docker     *docker.Client
	supervisor *supervisor.Supervisor
}

func New(st *store.Store, dc *docker.Client, sup *supervisor.Supervisor) *Reconciler {
	return &Reconciler{store: st, docker: dc, supervisor: sup}
}

// Result summarises what the reconciler did. Surfaced for tests and logs.
type Result struct {
	Reattached        int
	MarkedResumable   int
	MarkedUnresumable int
	ResumeScheduled   int
}

// Run performs the reconciliation. autoResume controls whether the supervisor
// is asked to immediately resume jobs that landed in interrupted_resumable.
func (r *Reconciler) Run(ctx context.Context, autoResume bool) (Result, error) {
	res := Result{}
	pendingJobs, err := r.store.JobsInStates(ctx, store.StateRunning, store.StateCheckpointing, store.StatePending, store.StateInterruptedResumable)
	if err != nil {
		return res, err
	}

	live, err := r.docker.ListByLabel(ctx, docker.LabelJobID, "")
	if err != nil {
		// No daemon = nothing to reconcile against; treat all running rows as
		// crashed. Continue with an empty live set.
		log.Printf("recovery: ListByLabel: %v", err)
		live = nil
	}
	liveByJobID := map[string]docker.ContainerSummary{}
	for _, c := range live {
		jid := c.Labels[docker.LabelJobID]
		if jid == "" {
			continue
		}
		// Prefer the running container if multiple share a job id.
		existing, ok := liveByJobID[jid]
		if !ok || c.State == "running" && existing.State != "running" {
			liveByJobID[jid] = c
		}
	}

	currentPID := strconv.Itoa(r.supervisor.PID())
	for _, j := range pendingJobs {
		summary, hasContainer := liveByJobID[j.ID]
		switch j.State {
		case store.StateRunning, store.StateCheckpointing:
			if hasContainer && summary.State == "running" && summary.Labels[docker.LabelControllerPID] != currentPID {
				log.Printf("recovery: re-attaching to job=%s container=%s", j.ID, summary.ID)
				r.supervisor.AdoptRunning(ctx, j.ID, summary.ID)
				res.Reattached++
				continue
			}
			if !hasContainer || summary.State != "running" {
				if j.LastCheckpointAt != nil {
					_ = r.store.Transition(ctx, j.ID, store.StateInterruptedResumable)
					res.MarkedResumable++
					if autoResume {
						if err := r.supervisor.Start(ctx, j.ID); err != nil {
							log.Printf("recovery: resume %s: %v", j.ID, err)
						} else {
							res.ResumeScheduled++
						}
					}
				} else {
					_ = r.store.Transition(ctx, j.ID, store.StateInterruptedUnresumable)
					res.MarkedUnresumable++
				}
			}
		case store.StateInterruptedResumable:
			if autoResume {
				if err := r.supervisor.Start(ctx, j.ID); err != nil {
					log.Printf("recovery: resume %s: %v", j.ID, err)
				} else {
					res.ResumeScheduled++
				}
			}
		case store.StatePending:
			// Pending jobs with no container were never actually started; leave them.
		}
	}
	return res, nil
}
