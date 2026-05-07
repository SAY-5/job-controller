// Package docker is a thin wrapper over the Docker engine API used by the
// supervisor. The label conventions documented here are the contract used by
// the recovery scan to find and re-attach to orphan worker containers.
package docker

const (
	// LabelJobID is the controller-assigned UUID for a job.
	LabelJobID = "com.jobctl.job_id"

	// LabelControllerPID is the OS PID of the controller that spawned the
	// container. Used by the recovery scan to identify orphans.
	LabelControllerPID = "com.jobctl.controller_pid"

	// LabelCreatedAt is the Unix-nano time the container was launched.
	LabelCreatedAt = "com.jobctl.created_at"
)
