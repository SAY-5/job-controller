package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{
		"JOBCTL_LISTEN", "JOBCTL_DB", "JOBCTL_WORKER_IMAGE",
		"JOBCTL_WORKER_STATE_PATH", "JOBCTL_HOST_STATE_DIR",
		"JOBCTL_GRACE_PERIOD", "JOBCTL_RECONCILE_EVERY", "JOBCTL_NO_DOCKER",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg := Load()
	if cfg.Listen != ":8080" {
		t.Fatalf("Listen = %q want :8080", cfg.Listen)
	}
	if cfg.DBPath == "" {
		t.Fatalf("DBPath empty")
	}
	if cfg.GracePeriod != 30*time.Second {
		t.Fatalf("GracePeriod = %v", cfg.GracePeriod)
	}
	if cfg.ReconcileEvery != 10*time.Second {
		t.Fatalf("ReconcileEvery = %v", cfg.ReconcileEvery)
	}
	if cfg.NoDocker {
		t.Fatalf("NoDocker default should be false")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("JOBCTL_LISTEN", ":9090")
	t.Setenv("JOBCTL_DB", "/tmp/x.db")
	t.Setenv("JOBCTL_WORKER_IMAGE", "test/img:1")
	t.Setenv("JOBCTL_GRACE_PERIOD", "5s")
	t.Setenv("JOBCTL_RECONCILE_EVERY", "1s")
	t.Setenv("JOBCTL_NO_DOCKER", "true")
	cfg := Load()
	if cfg.Listen != ":9090" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}
	if cfg.DBPath != "/tmp/x.db" {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.DefaultImage != "test/img:1" {
		t.Fatalf("DefaultImage = %q", cfg.DefaultImage)
	}
	if cfg.GracePeriod != 5*time.Second {
		t.Fatalf("GracePeriod = %v", cfg.GracePeriod)
	}
	if cfg.ReconcileEvery != time.Second {
		t.Fatalf("ReconcileEvery = %v", cfg.ReconcileEvery)
	}
	if !cfg.NoDocker {
		t.Fatalf("NoDocker = false")
	}
}

func TestLoadInvalidDurationFallsBack(t *testing.T) {
	t.Setenv("JOBCTL_GRACE_PERIOD", "not-a-duration")
	cfg := Load()
	if cfg.GracePeriod != 30*time.Second {
		t.Fatalf("expected fallback default, got %v", cfg.GracePeriod)
	}
}

func TestLoadInvalidBoolFallsBack(t *testing.T) {
	t.Setenv("JOBCTL_NO_DOCKER", "yesplease")
	cfg := Load()
	if cfg.NoDocker {
		t.Fatalf("invalid bool should fall back to default false")
	}
}
