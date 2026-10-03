# docker-bouncer

Zero-downtime rolling deploys for Docker Compose on a single host: an Envoy
proxy in front of each service and health-gated bounces, as a Docker CLI plugin.

<p align="center">
  <img src="docs/how-it-works.svg" width="720" alt="Animation: the api proxy (Envoy) sends traffic to replica api-app-1 running v1. A new replica api-app-2 running v2 starts, becomes healthy and joins Envoy's list; api-app-1 is taken off the list, finishes its in-flight requests and is removed.">
</p>

## Why

- **Zero-downtime deploys for Compose on one host.** Change the image, run
  `docker bouncer up`, and new replicas take traffic only once healthy while
  old ones drain.
- **Kubernetes-style Services, [PaaSTA](https://github.com/yelp/paasta)-style bounces.** A stable address
  (name, ports, aliases) in front of the healthy replicas, and four bounce
  methods: `crossover`, `upthendown`, `downthenup`, `brutal`.
- **No control plane, no state store.** Every run reads the compose file,
  container labels and Envoy's live view. Revisions live in the replicas'
  labels, so `history` and `undo` work without a database.

## Install

Linux or macOS, amd64 or arm64; downloads the latest release as a Docker CLI plugin:

```sh
mkdir -p ~/.docker/cli-plugins
curl -fsSL -o ~/.docker/cli-plugins/docker-bouncer \
  "https://github.com/cuza/docker-bouncer/releases/latest/download/docker-bouncer-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
chmod +x ~/.docker/cli-plugins/docker-bouncer
docker bouncer --help
```

Checksums are in each release's `SHA256SUMS`. From source:
`go build -o ~/.docker/cli-plugins/docker-bouncer .`

## Quick start

Add an `x-bouncer` block to an HTTP service. Envoy health-checks `GET /`
on the first port (any status below 500 passes; an explicit `healthcheck.uri`
must answer 2xx); the Docker `healthcheck` is optional.

```yaml
services:
  api:
    image: registry/app:1.0
    deploy: { replicas: 2 }
    restart: unless-stopped
    ports: ["127.0.0.1:8080:8080"]
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:8080/health"]
      interval: 5s
    x-bouncer: {}
```

```sh
docker bouncer up        # creates the proxy "api" and the replicas "api-app"
# change image: to registry/app:1.1, then:
docker bouncer up        # bounces api: new replicas in, old ones drained
docker bouncer history api
docker bouncer undo      # bounces back to registry/app:1.0
```

## How it works

<p align="center">
  <img src="docs/bounce-crossover.svg" width="760" alt="Animation: crossover with three replicas. app-4 (v2) starts, joins Envoy's list and turns healthy, then app-1 (v1) leaves the list, finishes its in-flight requests and is removed; app-5 and app-6 replace app-2 and app-3 the same way. Three replicas serve throughout and at most four exist.">
</p>

Each Service `api` becomes an Envoy proxy named `api` (it keeps the ports,
networks and aliases) plus replicas `api-app`. Bouncer drives the proxy with
`docker exec`, rewriting its cluster list and reading its health checks. The
proxy appends the caller's address to `X-Forwarded-For` and keeps an incoming
`X-Forwarded-Proto`. A
lock container `<project>-bouncer-lock`, created but never started, keeps two
runs apart.

## Commands

Global flags as Compose: `-f/--file`, `-p/--project-name`,
`--project-directory`, `--profile`, `--env-file`, and
`--progress auto|tty|plain|json|quiet` (Compose's progress display; `json` is
one object per line for CI, `quiet` prints errors only). `--timestamps`
prefixes `plain` lines with an RFC 3339 UTC time.

| Command | Does |
|---|---|
| `up [SERVICE…] [--pull …] [--force-unlock] [-d] [--wait]` | Pull, converge plain services, bounce changed Services; always detached and waits for convergence (`-d`, `--wait` accepted for compatibility) |
| `undo [SERVICE] [--to-revision N]` | Bounce back to a stored revision |
| `history SERVICE` | Stored revisions |
| `ps` | Containers with role and revision |
| `ls` | Projects with Services on this host |
| `logs SERVICE [--follow] [--proxy] [-n N]` | Logs of all replicas, or the proxy |
| `pull [SERVICE…]` | Pull images, including the proxy image |
| `config` | The derived project |
| `stop [SERVICE…] [-t N]` | Stop containers; no drain |
| `down` | Remove the project and its revision history |
| `version [--short]` (or `--version`) | Show the version, plus the Compose library and default Envoy image it is built on |

## Documentation

The [wiki](https://github.com/cuza/docker-bouncer/wiki) has the details:

- [Getting started](https://github.com/cuza/docker-bouncer/wiki/Getting-started)
- [Configuration](https://github.com/cuza/docker-bouncer/wiki/Configuration): every `x-bouncer` key
- [Bounce methods](https://github.com/cuza/docker-bouncer/wiki/Bounce-methods): animations, surge and margin
- [Draining and health](https://github.com/cuza/docker-bouncer/wiki/Draining-and-health)
- [Operations](https://github.com/cuza/docker-bouncer/wiki/Operations): `up`, `undo`, locks, reboots, output and CI
- [Troubleshooting](https://github.com/cuza/docker-bouncer/wiki/Troubleshooting): exit codes, known gaps
- [How it works](https://github.com/cuza/docker-bouncer/wiki/How-it-works): derived services, labels, Envoy

## License

Apache-2.0
