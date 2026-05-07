package supervisor

// CheckpointEvent mirrors the JSON line produced by the C++ worker.
type CheckpointEvent struct {
	Type      string  `json:"type"`
	JobID     string  `json:"job_id"`
	Epoch     int64   `json:"epoch"`
	Progress  float64 `json:"progress"`
	Found     int64   `json:"found"`
	Next      int64   `json:"next"`
	StatePath string  `json:"state_path"`
	Recent    []int64 `json:"recent"`
}

// StartedEvent is emitted exactly once per worker process at startup.
type StartedEvent struct {
	Type            string `json:"type"`
	JobID           string `json:"job_id"`
	Limit           int64  `json:"limit"`
	ResumeFromEpoch int64  `json:"resume_from_epoch"`
}

// CompletedEvent is emitted exactly once per successful run.
type CompletedEvent struct {
	Type  string `json:"type"`
	JobID string `json:"job_id"`
	Found int64  `json:"found"`
	Epoch int64  `json:"epoch"`
}
