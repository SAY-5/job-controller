// Package signals owns the controller's signal handlers.
//
// The signal contract:
//
//	SIGTERM  graceful shutdown: broadcast SIGTERM to workers, wait grace,
//	         kill survivors, mark interrupted, exit.
//	SIGHUP   reload config (controller only; workers continue).
//	SIGUSR1  simulated hardware-fault: immediate snapshot + safe shutdown.
//	         In production this would be wired to mcelog / EDAC.
package signals

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/SAY-5/job-controller/internal/supervisor"
)

// Handler holds the pieces a signal action needs.
type Handler struct {
	Supervisor *supervisor.Supervisor
	Reload     func()
	OnExit     func()
}

// Run installs handlers and blocks until ctx is cancelled or the process
// receives a terminating signal. Returns when shutdown is complete.
func Run(ctx context.Context, h Handler) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			log.Printf("signals: context cancelled, draining workers")
			h.Supervisor.ShutdownGraceful(context.Background(), false)
			if h.OnExit != nil {
				h.OnExit()
			}
			return
		case sig := <-ch:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				log.Printf("signals: %s received; graceful shutdown", sig)
				h.Supervisor.ShutdownGraceful(context.Background(), false)
				if h.OnExit != nil {
					h.OnExit()
				}
				return
			case syscall.SIGHUP:
				log.Printf("signals: SIGHUP received; reloading config")
				if h.Reload != nil {
					h.Reload()
				}
			case syscall.SIGUSR1:
				log.Printf("signals: SIGUSR1 received; simulated hardware fault, immediate shutdown")
				h.Supervisor.ShutdownGraceful(context.Background(), true)
				if h.OnExit != nil {
					h.OnExit()
				}
				return
			}
		}
	}
}
