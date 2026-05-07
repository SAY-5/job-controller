// Command controller is the supervising process. It owns the SQLite job log,
// runs the HTTP API, and manages worker containers.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/SAY-5/job-controller/internal/api"
	"github.com/SAY-5/job-controller/internal/cluster"
	"github.com/SAY-5/job-controller/internal/config"
	"github.com/SAY-5/job-controller/internal/docker"
	"github.com/SAY-5/job-controller/internal/recovery"
	"github.com/SAY-5/job-controller/internal/registry"
	"github.com/SAY-5/job-controller/internal/signals"
	"github.com/SAY-5/job-controller/internal/store"
	"github.com/SAY-5/job-controller/internal/supervisor"
)

func main() {
	cfg := config.Load()
	log.Printf("controller: starting pid=%d listen=%s db=%s image=%s",
		os.Getpid(), cfg.Listen, cfg.DBPath, cfg.DefaultImage)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("controller: open store: %v", err)
	}
	defer st.Close()

	var dc *docker.Client
	if !cfg.NoDocker {
		dc, err = docker.NewClient()
		if err != nil {
			log.Fatalf("controller: docker client: %v", err)
		}
	}

	sup := supervisor.New(cfg, st, dc)

	// HA / leader election: if a Redis address is configured the
	// controller joins the cluster ring and only schedules when leader.
	// Without a Redis address the supervisor's leaderCheck stays nil
	// and the controller behaves as a standalone single-node instance.
	ctxRoot, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	if cfg.RedisAddr != "" {
		ctlID := cfg.ControllerID
		if ctlID == "" {
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			ctlID = "ctl-" + hex.EncodeToString(b)
		}
		locker := cluster.NewRedisLocker(cfg.RedisAddr)
		electCfg := cluster.Config{
			Key:          cfg.ClusterKey,
			ControllerID: ctlID,
			LeaseTTL:     cfg.LeaseTTL,
			RefreshEvery: cfg.RefreshEvery,
			PollEvery:    cfg.PollEvery,
		}
		elector := cluster.New(electCfg, locker,
			func() { log.Printf("controller: became leader id=%s", ctlID) },
			func() { log.Printf("controller: lost leadership id=%s", ctlID) })
		sup.SetLeaderCheck(ctlID, elector.IsLeader)
		go elector.Run(ctxRoot)
		log.Printf("controller: HA mode redis=%s id=%s lease=%s", cfg.RedisAddr, ctlID, cfg.LeaseTTL)
	}

	// Recovery scan: re-attach orphans, mark dead jobs.
	rec := recovery.New(st, dc, sup)
	res, err := rec.Run(context.Background(), true)
	if err != nil {
		log.Printf("controller: recovery scan: %v", err)
	} else {
		log.Printf("controller: recovery: reattached=%d resumable=%d unresumable=%d resume_scheduled=%d",
			res.Reattached, res.MarkedResumable, res.MarkedUnresumable, res.ResumeScheduled)
	}

	// Worker registry: try worker_registry.yaml in CWD, fall back to the
	// embedded default. Logged either way so operators can confirm.
	reg, regErr := registry.LoadFile("worker_registry.yaml")
	if regErr != nil {
		log.Printf("controller: worker_registry.yaml not loaded (%v); using embedded default", regErr)
		reg = registry.Default()
	} else {
		log.Printf("controller: worker registry loaded: %v", reg.Names())
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.NewServer(cfg, st, dc, sup, reg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("controller: http listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("controller: http: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals.Run(ctx, signals.Handler{
		Supervisor: sup,
		Reload: func() {
			// Cheap reload: re-read env into a new config and log delta.
			next := config.Load()
			log.Printf("controller: reloaded config grace=%s reconcile=%s", next.GracePeriod, next.ReconcileEvery)
		},
		OnExit: func() {
			shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = srv.Shutdown(shutdownCtx)
		},
	})

	log.Printf("controller: shutdown complete")
}
