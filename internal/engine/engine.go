// Package engine adapts the Docker API and the Envoy proxy containers to the
// small surface the bounce planner and runner need.
package engine

import (
	"context"
	"time"
)

type Replica struct {
	ID, Name     string
	IPs          []string
	Running      bool
	DockerHealth string // "", "starting", "healthy", "unhealthy"
	Created      time.Time
	Labels       map[string]string
}

type Engine interface {
	Replicas(ctx context.Context, project, service string) ([]Replica, error)                  // role=replica, bouncer.service=service, all states
	Container(ctx context.Context, project string, labels map[string]string) (*Replica, error) // first match or nil
	Stop(ctx context.Context, id string) error                                                 // container's own stop timeout
	Remove(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, env []string, cmd ...string) (string, error) // stdout; non-zero exit → error with stderr
	Wait(ctx context.Context, project string, d time.Duration)                        // returns on the project's next container event or after d
}
