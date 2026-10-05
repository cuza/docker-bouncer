package revision

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// everyField is a service using every attribute of the service schema, with
// the forms compose-go's JSON cannot read back with its own types (extra_hosts
// as a list and a map, short ulimits, secret and config modes, short-syntax
// binds whose create_host_path defaults to true).
const everyField = `
services:
  api:
    annotations: {a: "1"}
    attach: false
    blkio_config:
      weight: 300
      weight_device: [{path: /dev/sda, weight: 400}]
      device_read_bps: [{path: /dev/sda, rate: 1mb}]
      device_read_iops: [{path: /dev/sda, rate: 30}]
      device_write_bps: [{path: /dev/sda, rate: 1mb}]
      device_write_iops: [{path: /dev/sda, rate: 30}]
    build:
      context: .
      dockerfile: Dockerfile
      args: {A: "1"}
      ssh: [default]
      labels: {l: "1"}
      cache_from: [registry/app:cache]
      cache_to: [type=inline]
      no_cache: true
      additional_contexts: {other: ./other}
      network: host
      pull: true
      target: prod
      shm_size: 64m
      extra_hosts: ["build.internal:10.0.0.1"]
      isolation: default
      privileged: true
      secrets: [{source: token, target: /run/token, mode: 0400}]
      tags: [registry/app:extra]
      ulimits: {nofile: {soft: 10, hard: 20}}
      platforms: [linux/amd64]
      provenance: "false"
      sbom: "false"
      entitlements: [network.host]
    cap_add: [NET_ADMIN]
    cap_drop: [ALL]
    cgroup: host
    cgroup_parent: parent
    command: ["serve", "--port", "8080"]
    configs:
      - cfg
      - {source: cfg, target: /etc/cfg, uid: "1", gid: "2", mode: 0440}
    container_name: api-fixed
    cpu_count: 2
    cpu_percent: 50
    cpu_period: 100000
    cpu_quota: 50000
    cpu_rt_period: 1000
    cpu_rt_runtime: 500
    cpus: 1.5
    cpuset: "0-1"
    cpu_shares: 512
    credential_spec: {config: cfg}
    depends_on:
      db: {condition: service_healthy, restart: true, required: false}
    deploy:
      mode: replicated
      replicas: 2
      labels: {d: "1"}
      update_config: {parallelism: 1, delay: 5s, order: start-first}
      resources:
        limits: {cpus: "0.5", memory: 64m, pids: 100}
        reservations:
          cpus: "0.25"
          memory: 32m
          devices: [{capabilities: [gpu], count: 1}]
      restart_policy: {condition: on-failure, delay: 5s, max_attempts: 3, window: 1m}
      placement: {constraints: [node.role==worker]}
    develop:
      watch: [{path: ./src, action: sync, target: /app/src, ignore: [x]}]
    device_cgroup_rules: ["c 1:3 mr"]
    devices: ["/dev/null:/dev/null:rwm"]
    dns: 8.8.8.8
    dns_opt: [use-vc]
    dns_search: example.internal
    dockerfile: Dockerfile.alt
    domainname: example.internal
    entrypoint: /entry.sh
    environment: {PORT: "8080", EMPTY: ""}
    env_file: [{path: ./app.env, required: false, format: raw}]
    expose: ["8080", "9090"]
    external_links: [other:alias]
    extra_hosts:
      - host.docker.internal:host-gateway
      - "multi=10.0.0.1"
      - "multi=10.0.0.2"
    gpus: [{driver: nvidia, count: 1}]
    group_add: [audio]
    healthcheck:
      test: ["CMD", "true"]
      interval: 10s
      timeout: 2s
      retries: 3
      start_period: 1s
      start_interval: 1s
    hostname: api
    image: registry/app:1
    init: true
    ipc: shareable
    isolation: default
    labels: {app: api}
    label_file: [./labels.env]
    links: [db:database]
    logging: {driver: json-file, options: {max-size: 1m}}
    log_driver: json-file
    log_opt: {max-file: "3"}
    mac_address: "02:42:ac:11:00:02"
    mem_limit: 128m
    mem_reservation: 64m
    memswap_limit: 256m
    mem_swappiness: 10
    models: {llm: {endpoint_var: LLM_URL, model_var: LLM_MODEL}}
    net: none
    network_mode: bridge
    networks:
      front:
        aliases: [api-alias]
        ipv4_address: 172.30.0.10
        ipv6_address: "fd00::10"
        link_local_ips: [169.254.0.10]
        mac_address: "02:42:ac:11:00:03"
        driver_opts: {o: "1"}
        priority: 10
        gw_priority: 1
        interface_name: eth9
    oom_kill_disable: true
    oom_score_adj: 100
    pid: host
    pids_limit: 100
    platform: linux/amd64
    ports:
      - "8080:80"
      - {target: 443, published: "8443", protocol: tcp, mode: host, host_ip: 127.0.0.1, app_protocol: https, name: tls}
    post_start: [{command: ["true"], user: root, privileged: true, working_dir: /, environment: {A: "1"}}]
    pre_stop: [{command: ["true"]}]
    pre_start: [{command: ["migrate"], image: registry/app:1, user: root, environment: {A: "1"}}]
    privileged: true
    profiles: [all]
    provider: {type: model, options: {model: ai/x, flags: [a, b]}}
    pull_policy: always
    pull_refresh_after: 1h
    read_only: true
    restart: always
    runtime: runc
    scale: 2
    secrets:
      - token
      - {source: token, target: /run/secret, uid: "1", gid: "2", mode: 0400}
    security_opt: [no-new-privileges]
    shm_size: 64m
    stdin_open: true
    stop_grace_period: 1m30s
    stop_signal: SIGINT
    storage_opt: {size: 1G}
    sysctls: {net.core.somaxconn: "1024"}
    tmpfs: [/tmp]
    tty: true
    ulimits:
      nproc: 65535
      nofile: {soft: 20000, hard: 40000}
    use_api_socket: true
    user: "1000:1000"
    userns_mode: host
    uts: host
    volume_driver: local
    volumes:
      - ./data:/data
      - ./ro:/ro:ro,z
      - type: bind
        source: ./nocreate
        target: /nocreate
        bind: {create_host_path: false, propagation: rshared, recursive: enabled}
      - {type: volume, source: vol, target: /vol, volume: {nocopy: true, subpath: sub, labels: {v: "1"}}}
      - {type: tmpfs, target: /scratch, tmpfs: {size: 1m, mode: 0700}}
      - {type: image, source: registry/data:1, target: /img, image: {subpath: sub}}
    volumes_from: [db]
    working_dir: /app
    x-custom: kept
  db:
    image: registry/db:1
networks:
  front:
    enable_ipv6: true
volumes:
  vol: {}
secrets:
  token: {file: ./token}
configs:
  cfg: {file: ./cfg}
`

// unset are the ServiceConfig fields the fixture cannot carry: they are not
// part of the stored spec, or the loader consumes them.
var unset = map[string]bool{
	"Name": true, "CustomLabels": true, "Extends": true,
}

func loadEveryField(t *testing.T) types.ServiceConfig {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"app.env", "labels.env", "token", "cfg"} {
		write(t, filepath.Join(dir, f), "")
	}
	write(t, filepath.Join(dir, "labels.env"), "from_file=1\n")
	write(t, filepath.Join(dir, "app.env"), "SECRET=x\n")
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Join(dir, "compose.yaml"), Content: []byte(everyField)}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SetProjectName("everyfield", true)
		o.SkipConsistencyCheck = true
		o.SkipValidation = true // legacy keys (net, log_driver) too
		o.Profiles = []string{"all"}
	})
	if err != nil {
		t.Fatal(err)
	}
	s := p.Services["api"]
	s.Name = "api-app"
	return s
}

// fields lists every field of a struct, descending into embedded ones.
func fields(v reflect.Value, out map[string]reflect.Value) {
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Anonymous {
			fields(v.Field(i), out)
			continue
		}
		out[f.Name] = v.Field(i)
	}
}

func TestEveryFieldRoundTrips(t *testing.T) {
	a := loadEveryField(t)
	all := map[string]reflect.Value{}
	fields(reflect.ValueOf(a), all)
	for name, v := range all {
		if v.IsZero() && !unset[name] {
			t.Errorf("fixture leaves %s unset: add it to everyField", name)
		}
	}
	spec, hash, err := Encode(a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(spec)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want, _ := Strip(a)
	want.Name = ""
	want.Extensions = nil // x-* keys are not stored; Compose ignores them on a service
	if !reflect.DeepEqual(got, want) {
		gm, wm := map[string]reflect.Value{}, map[string]reflect.Value{}
		fields(reflect.ValueOf(got), gm)
		fields(reflect.ValueOf(want), wm)
		for name := range wm {
			if !reflect.DeepEqual(gm[name].Interface(), wm[name].Interface()) {
				t.Errorf("%s: got %#v, want %#v", name, gm[name].Interface(), wm[name].Interface())
			}
		}
	}
	// Re-encoding what was read back gives the same spec: undo of an undo
	// is a no-op, and the hash of a restored spec matches.
	if _, h2, err := Encode(got); err != nil || h2 != hash {
		t.Errorf("re-encoded hash %s, want %s (%v)", h2, hash, err)
	}
}
