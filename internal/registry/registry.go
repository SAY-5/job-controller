// Package registry parses worker_registry.yaml -- the declarative list of
// known worker types -- and exposes lookup by worker_name.
//
// The parser intentionally implements only the subset of YAML the registry
// uses: a top-level `workers:` list of dictionaries with scalar string and
// integer values. This avoids pulling a generic YAML dep into go.mod for a
// 30-line config. Unknown keys are ignored; missing required keys produce
// a parse error.
package registry

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Worker is one entry from the registry.
type Worker struct {
	Name               string
	Image              string
	Command            string
	CheckpointInterval int
	SchemaVersion      int
}

// Registry is a name-keyed lookup over the parsed entries.
type Registry struct {
	byName map[string]Worker
	order  []string
}

var (
	// ErrUnknownWorker is returned by Lookup when no entry matches.
	ErrUnknownWorker = errors.New("registry: unknown worker_name")
	// ErrInvalidSchema is returned when an entry is missing required fields.
	ErrInvalidSchema = errors.New("registry: invalid worker entry")
)

// Default returns the registry built into the binary. It is used when no
// worker_registry.yaml file is present on disk; matches the spec defaults.
func Default() *Registry {
	r := &Registry{byName: map[string]Worker{}}
	for _, w := range []Worker{
		{Name: "primes", Image: "jobctl/worker:latest", Command: "/usr/local/bin/jobworker", CheckpointInterval: 5000, SchemaVersion: 2},
		{Name: "matmul", Image: "jobctl/worker:latest", Command: "/usr/local/bin/jobworker", CheckpointInterval: 1000, SchemaVersion: 1},
		{Name: "wordcount", Image: "jobctl/worker:latest", Command: "/usr/local/bin/jobworker", CheckpointInterval: 1000, SchemaVersion: 1},
	} {
		r.byName[w.Name] = w
		r.order = append(r.order, w.Name)
	}
	return r
}

// LoadFile parses a registry from the given path. Falls back to Default
// (with a warning printed by the caller) if the file is missing.
func LoadFile(path string) (*Registry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads YAML from r. The format is:
//
//	workers:
//	  - worker_name: ...
//	    image: ...
//	    command: ...
//	    checkpoint_interval: 1000
//	    schema_version: 1
//	  - worker_name: ...
func Parse(r io.Reader) (*Registry, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	out := &Registry{byName: map[string]Worker{}}

	var seenWorkers bool
	var current *Worker
	for ln, raw := range lines {
		line := raw
		// Strip comments.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		// Top-level `workers:` marker.
		if !strings.HasPrefix(trimmed, " ") && strings.HasPrefix(strings.TrimSpace(trimmed), "workers:") {
			seenWorkers = true
			continue
		}
		if !seenWorkers {
			continue
		}
		// New list item: leading spaces, then "- key: value".
		ts := strings.TrimLeft(trimmed, " \t")
		if strings.HasPrefix(ts, "- ") {
			// commit previous entry.
			if current != nil {
				if err := commit(out, current, ln); err != nil {
					return nil, err
				}
			}
			current = &Worker{}
			ts = strings.TrimSpace(ts[2:])
			if ts == "" {
				continue
			}
			if err := setField(current, ts); err != nil {
				return nil, fmt.Errorf("line %d: %w", ln+1, err)
			}
			continue
		}
		// Continuation field of current item.
		if current == nil {
			return nil, fmt.Errorf("line %d: field outside of any list item", ln+1)
		}
		if err := setField(current, ts); err != nil {
			return nil, fmt.Errorf("line %d: %w", ln+1, err)
		}
	}
	if current != nil {
		if err := commit(out, current, len(lines)); err != nil {
			return nil, err
		}
	}
	if len(out.byName) == 0 {
		return nil, fmt.Errorf("registry: no workers defined")
	}
	return out, nil
}

func commit(r *Registry, w *Worker, ln int) error {
	if w.Name == "" {
		return fmt.Errorf("line %d: %w: missing worker_name", ln, ErrInvalidSchema)
	}
	if w.Image == "" {
		return fmt.Errorf("line %d: %w: missing image for %s", ln, ErrInvalidSchema, w.Name)
	}
	if w.Command == "" {
		return fmt.Errorf("line %d: %w: missing command for %s", ln, ErrInvalidSchema, w.Name)
	}
	if _, dup := r.byName[w.Name]; dup {
		return fmt.Errorf("line %d: duplicate worker_name %q", ln, w.Name)
	}
	r.byName[w.Name] = *w
	r.order = append(r.order, w.Name)
	return nil
}

func setField(w *Worker, kv string) error {
	colon := strings.Index(kv, ":")
	if colon < 0 {
		return fmt.Errorf("expected key:value, got %q", kv)
	}
	key := strings.TrimSpace(kv[:colon])
	val := strings.TrimSpace(kv[colon+1:])
	val = strings.Trim(val, "\"'")
	switch key {
	case "worker_name":
		w.Name = val
	case "image":
		w.Image = val
	case "command":
		w.Command = val
	case "checkpoint_interval":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("checkpoint_interval %q: %w", val, err)
		}
		w.CheckpointInterval = n
	case "schema_version":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("schema_version %q: %w", val, err)
		}
		w.SchemaVersion = n
	}
	// Unknown keys are silently ignored to keep the format forward-compatible.
	return nil
}

// Lookup returns the worker entry for `name`. Returns ErrUnknownWorker on
// miss.
func (r *Registry) Lookup(name string) (Worker, error) {
	w, ok := r.byName[name]
	if !ok {
		return Worker{}, fmt.Errorf("%w: %s", ErrUnknownWorker, name)
	}
	return w, nil
}

// Names returns the registered worker names in insertion order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}
