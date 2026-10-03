// Package lock is a mutex made of Docker's unique container names.
package lock

import (
	"context"
	"errors"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const LabelLockOwner = "bouncer.lock-owner"

var ErrConflict = errors.New("name in use")

type ErrHeld struct {
	Owner string
	Since time.Time
}

func (e ErrHeld) Error() string {
	return fmt.Sprintf("bounce in progress by %s since %s (use --force-unlock if it is gone)", e.Owner, e.Since.Format(time.RFC3339))
}

// Locker creates, inspects and removes the lock container. Remove takes the
// container ID so a takeover can never delete a newer holder's lock.
// Inspect returns an empty id when the name is free.
type Locker interface {
	Create(ctx context.Context, name, image string, labels map[string]string) (id string, err error)
	Inspect(ctx context.Context, name string) (id string, created time.Time, labels map[string]string, err error)
	Remove(ctx context.Context, id string) error
}

func Acquire(ctx context.Context, l Locker, project, image, owner string, staleAfter time.Duration, force bool, now time.Time) (func(context.Context) error, error) {
	name := project + "-bouncer-lock"
	labels := map[string]string{
		transform.LabelRole: transform.RoleLock, LabelLockOwner: owner, "com.docker.compose.project": project,
	}
	for attempt := 0; attempt < 2; attempt++ {
		id, err := l.Create(ctx, name, image, labels)
		if err == nil {
			return func(ctx context.Context) error { return l.Remove(ctx, id) }, nil
		}
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		heldID, created, held, err := l.Inspect(ctx, name)
		if err != nil {
			return nil, err
		}
		if heldID == "" {
			continue // released between Create and Inspect
		}
		if !force && now.Sub(created) < staleAfter {
			return nil, ErrHeld{Owner: held[LabelLockOwner], Since: created}
		}
		if err := l.Remove(ctx, heldID); err != nil {
			return nil, err
		}
	}
	// Lost a race to another taker: report whoever holds it now.
	_, created, held, err := l.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	return nil, ErrHeld{Owner: held[LabelLockOwner], Since: created}
}

type docker struct{ c client.APIClient }

// NewDocker locks with a never-started container; image must be present locally.
func NewDocker(c client.APIClient) Locker { return &docker{c: c} }

func (d *docker) Create(ctx context.Context, name, image string, labels map[string]string) (string, error) {
	res, err := d.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name, Config: &container.Config{Image: image, Cmd: []string{"true"}, Labels: labels},
	})
	switch {
	case cerrdefs.IsConflict(err):
		return "", ErrConflict
	case cerrdefs.IsNotFound(err):
		return "", fmt.Errorf("lock image %s is not present locally, pull it first: %w", image, err)
	case err != nil:
		return "", err
	}
	return res.ID, nil
}

func (d *docker) Inspect(ctx context.Context, name string) (string, time.Time, map[string]string, error) {
	res, err := d.c.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return "", time.Time{}, nil, nil
	}
	if err != nil {
		return "", time.Time{}, nil, err
	}
	c := res.Container
	created, err := time.Parse(time.RFC3339Nano, c.Created)
	if err != nil {
		return "", time.Time{}, nil, fmt.Errorf("lock %s: created time: %w", name, err)
	}
	var labels map[string]string
	if c.Config != nil {
		labels = c.Config.Labels
	}
	return c.ID, created, labels, nil
}

func (d *docker) Remove(ctx context.Context, id string) error {
	_, err := d.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil // already gone
	}
	return err
}
