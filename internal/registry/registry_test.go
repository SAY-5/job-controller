package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultHasThreeBundledWorkers(t *testing.T) {
	r := Default()
	for _, name := range []string{"primes", "matmul", "wordcount"} {
		w, err := r.Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if w.Image == "" || w.Command == "" {
			t.Fatalf("worker %s missing image/command: %+v", name, w)
		}
	}
}

func TestParseValid(t *testing.T) {
	yaml := `workers:
  - worker_name: alpha
    image: ex/alpha:1
    command: /bin/alpha
    checkpoint_interval: 100
    schema_version: 1

  - worker_name: beta
    image: ex/beta:2
    command: /bin/beta
    checkpoint_interval: 200
    schema_version: 3
`
	r, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, err := r.Lookup("alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if a.Image != "ex/alpha:1" || a.CheckpointInterval != 100 || a.SchemaVersion != 1 {
		t.Fatalf("alpha = %+v", a)
	}
	b, err := r.Lookup("beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if b.Image != "ex/beta:2" || b.CheckpointInterval != 200 {
		t.Fatalf("beta = %+v", b)
	}
	if names := r.Names(); len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("Names() = %v", names)
	}
}

func TestParseRejectsMissingFields(t *testing.T) {
	yaml := `workers:
  - worker_name: only_name
`
	if _, err := Parse(strings.NewReader(yaml)); err == nil {
		t.Fatalf("expected error for missing image/command")
	} else if !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("expected ErrInvalidSchema, got %v", err)
	}
}

func TestLookupMissReturnsErr(t *testing.T) {
	r := Default()
	_, err := r.Lookup("nope")
	if !errors.Is(err, ErrUnknownWorker) {
		t.Fatalf("err = %v want ErrUnknownWorker", err)
	}
}

func TestLoadFileFromCommittedRegistry(t *testing.T) {
	// Walk up from this test's CWD to find worker_registry.yaml.
	candidates := []string{
		filepath.Join("..", "..", "worker_registry.yaml"),
		filepath.Join("..", "..", "..", "worker_registry.yaml"),
		"worker_registry.yaml",
	}
	var path string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		t.Skip("worker_registry.yaml not found relative to test CWD")
	}
	r, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for _, expected := range []string{"primes", "matmul", "wordcount"} {
		if _, err := r.Lookup(expected); err != nil {
			t.Fatalf("expected %q in committed registry: %v", expected, err)
		}
	}
}
