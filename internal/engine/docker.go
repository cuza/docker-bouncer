package engine

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	"github.com/cuza/docker-bouncer/internal/transform"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

type docker struct{ c client.APIClient }

func NewDocker(c client.APIClient) Engine { return &docker{c: c} }

func (d *docker) list(ctx context.Context, project string, labels map[string]string) ([]Replica, error) {
	f := client.Filters{}.Add("label", "com.docker.compose.project="+project)
	for k, v := range labels {
		f = f.Add("label", k+"="+v)
	}
	res, err := d.c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	var out []Replica
	for _, s := range res.Items {
		in, err := d.c.ContainerInspect(ctx, s.ID, client.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue // removed between list and inspect
		}
		if err != nil {
			return nil, err
		}
		c := in.Container
		r := Replica{ID: c.ID, Name: trimSlash(c.Name), Running: c.State != nil && c.State.Running, Labels: c.Config.Labels}
		if c.State != nil && c.State.Health != nil {
			r.DockerHealth = string(c.State.Health.Status)
		}
		r.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
		if c.State != nil {
			r.ExitCode = c.State.ExitCode
			r.Started, _ = time.Parse(time.RFC3339Nano, c.State.StartedAt) // zero on failure
		}
		if c.NetworkSettings != nil {
			for _, n := range c.NetworkSettings.Networks {
				if n != nil && n.IPAddress.IsValid() {
					r.IPs = append(r.IPs, n.IPAddress.String())
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (d *docker) Replicas(ctx context.Context, project, service string) ([]Replica, error) {
	return d.list(ctx, project, map[string]string{transform.LabelRole: transform.RoleReplica, transform.LabelService: service})
}

func (d *docker) Container(ctx context.Context, project string, labels map[string]string) (*Replica, error) {
	all, err := d.list(ctx, project, labels)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

func (d *docker) Start(ctx context.Context, id string) error {
	_, err := d.c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (d *docker) Stop(ctx context.Context, id string) error {
	_, err := d.c.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

func (d *docker) Remove(ctx context.Context, id string) error {
	// Its anonymous volumes go with it; named volumes are never removed here.
	_, err := d.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

func (d *docker) RemoveKeepVolumes(ctx context.Context, id string) error {
	_, err := d.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

func (d *docker) Logs(ctx context.Context, id string, tail int) (string, error) {
	rc, err := d.c.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(tail)})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var out bytes.Buffer
	_, err = stdcopy.StdCopy(&out, &out, rc)
	return out.String(), err
}

func (d *docker) Image(ctx context.Context, ref string) (string, []string, error) {
	res, err := d.c.ImageInspect(ctx, ref)
	return res.ID, res.RepoDigests, err
}

func (d *docker) Exec(ctx context.Context, id string, env []string, cmd ...string) (string, error) {
	ex, err := d.c.ExecCreate(ctx, id, client.ExecCreateOptions{Cmd: cmd, Env: env, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", err
	}
	att, err := d.c.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil {
		return "", err
	}
	ins, err := d.c.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{})
	if err != nil {
		return "", err
	}
	if ins.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("exec %v in %s: exit %d: %s", cmd, id, ins.ExitCode, stderr.String())
	}
	return stdout.String(), nil
}

func (d *docker) Wait(ctx context.Context, project string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ev := d.c.Events(ctx, client.EventsListOptions{Filters: client.Filters{}.
		Add("type", "container").Add("label", "com.docker.compose.project="+project).
		Add("event", "create", "start", "die", "stop", "destroy", "health_status")})
	select {
	case <-ev.Messages:
	case <-ev.Err: // daemon unreachable: sleep out the timeout, callers poll in a loop
		<-ctx.Done()
	case <-ctx.Done():
	}
}

func trimSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}
