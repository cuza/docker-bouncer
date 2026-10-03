# docker-bouncer

A Docker CLI plugin that gives Compose services Kubernetes-style Services (a
stable address in front of healthy replicas, served by an Envoy proxy) and
rolling bounces (start the new version, gate it on health, drain the old one)
on a single host, without a control plane or a state store. Everything it
knows is derived on each run from the compose file, container labels and
Envoy's live view.

## Install

```sh
go build -o docker-bouncer .
cp docker-bouncer ~/.docker/cli-plugins/
docker bouncer --help
```

## Configuration

A service becomes a Bouncer Service by having an `x-bouncer` block. Replica
count is the standard `deploy.replicas`; ports are the service's existing
`ports:` (published) and `expose:` (internal).

```yaml
services:
  api:
    image: registry/api@sha256:…
    deploy: { replicas: 2 }
    ports:
      - "127.0.0.1:8080:8080"          # Envoy publishes 127.0.0.1:8080 → replicas :8080
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:8080/health"]
    x-bouncer:
      bounce_method: crossover
      bounce_margin_factor: 0.95
      bounce_overprovision_factor: 1.0
      min_task_uptime: 10s
      bounce_health_timeout: 300s
      healthcheck: { mode: http, uri: /health }
      drain_method: envoy
      drain_method_params: { delay: 95s }
      history_max: 3

  worker:
    image: registry/worker@sha256:…
    expose: ["8001"]                    # internal Service: Envoy listens on worker:8001
    x-bouncer: {}                       # all defaults
```

| Key | Default | Meaning |
|---|---|---|
| `bounce_method` | `crossover` | `crossover`, `upthendown`, `downthenup`, `brutal` |
| `bounce_margin_factor` | `0.95` | `maxUnavailable = floor(N × (1 − factor))` |
| `bounce_overprovision_factor` | `1.0` | `maxSurge = ceil(N × factor)` |
| `min_task_uptime` | `10s` | a new replica must stay healthy this long before it counts |
| `bounce_health_timeout` | `300s` | from creation until happy, per new replica |
| `healthcheck.mode` / `.uri` | `http` / `/health` | Envoy's active health check |
| `drain_method` | `envoy` | `envoy`, `http` (URL hooks), `noop` |
| `drain_method_params.delay` | `60s` | upper bound on a drain |
| `history_max` | `3` | previous revisions kept in labels |
| `proxy_image` | `envoyproxy/envoy:v1.39.1` | must be the Ubuntu-based image |

## Commands

Global flags as Compose: `-f`, `-p`, `--project-directory`, `--profile`,
`--env-file`. Exit code 1 means a bounce failed, 2 a configuration error.

| Command | Behaviour |
|---|---|
| `up [service…]` | Pull, converge plain services, bounce Services in parallel. Flags `--pull <policy>`, `--force-unlock`. |
| `pull [service…]` | Pull images and the proxy image; change nothing. |
| `undo [service] [--to-revision n]` | Bounce back to a previous revision kept in labels. |
| `history <service>` | Revision, time, image digest, up-id. |
| `ps` | Containers of the derived project with revision and role. |
| `logs <service> [-f]` | All replicas, prefixed; `--proxy` for Envoy. |
| `ls` | Projects on the host with Bouncer Services and their status. |
| `config` | The derived project, including generated Envoy config. |
| `stop [service…] [-t s]` | `compose stop` of the derived project, no drain. |
| `down` | `compose down` of the derived project; deletes history. |

## License

Apache-2.0
