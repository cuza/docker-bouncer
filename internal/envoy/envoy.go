// Package envoy renders the proxy's configuration and reads its admin output.
package envoy

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/cuza/docker-bouncer/internal/config"
)

const (
	ClusterDir  = "/etc/bouncer/dyn"
	ClusterFile = ClusterDir + "/cds.json"
)

type obj = map[string]any

func mustJSON(v any) string {
	b, err := json.Marshal(v) // map keys are sorted: output is deterministic
	if err != nil {
		panic(err)
	}
	return string(b)
}

func clusterName(port int) string { return fmt.Sprintf("port-%d", port) }

// targets is the distinct container ports in first-seen order: two published
// ports may map to one target, which still needs only one listener and cluster.
func targets(svc config.Service) []int {
	var out []int
	for _, p := range svc.Ports {
		if !slices.Contains(out, p.Target) {
			out = append(out, p.Target)
		}
	}
	return out
}

func SeedHost(svc config.Service) string { return svc.Name + "-app" }

// Bootstrap is the proxy's static config. It never contains replica names:
// it is part of the proxy container's definition and therefore its config hash.
func Bootstrap(svc config.Service) string {
	var listeners []any
	for _, port := range targets(svc) {
		name := clusterName(port)
		listeners = append(listeners, obj{
			"name":    name,
			"address": obj{"socket_address": obj{"address": "0.0.0.0", "port_value": port}},
			"filter_chains": []any{obj{"filters": []any{obj{
				"name": "envoy.filters.network.http_connection_manager",
				"typed_config": obj{
					"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
					"stat_prefix": name,
					// Append the caller's address to X-Forwarded-For. One trusted hop keeps
					// an incoming X-Forwarded-Proto (a TLS proxy in front sets https)
					// instead of overwriting it with the plain-HTTP scheme of this hop.
					"use_remote_address":           true,
					"xff_num_trusted_hops":         1,
					"preserve_external_request_id": true,
					"http_filters": []any{obj{
						"name":         "envoy.filters.http.router",
						"typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"},
					}},
					"route_config": obj{
						"validate_clusters": false,
						"virtual_hosts": []any{obj{
							"name": "all", "domains": []any{"*"},
							"routes": []any{obj{
								"match": obj{"prefix": "/"},
								"route": obj{
									"cluster": name,
									"timeout": "0s",
									"retry_policy": obj{
										"retry_on":    "connect-failure,refused-stream,reset-before-request",
										"num_retries": 3,
										"retry_host_predicate": []any{obj{
											"name":         "envoy.retry_host_predicates.previous_hosts",
											"typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.retry.host.previous_hosts.v3.PreviousHostsPredicate"},
										}},
										"host_selection_retry_max_attempts": 5,
									},
								},
							}},
						}},
					},
				},
			}}}},
		})
	}
	return mustJSON(obj{
		"node":  obj{"id": svc.Name, "cluster": svc.Name},
		"admin": obj{"address": obj{"socket_address": obj{"address": "127.0.0.1", "port_value": svc.AdminPort}}},
		"dynamic_resources": obj{"cds_config": obj{
			"resource_api_version": "V3",
			"path_config_source": obj{
				"path":              ClusterFile,
				"watched_directory": obj{"path": ClusterDir},
			},
		}},
		"static_resources": obj{"listeners": listeners},
	})
}

// Clusters is the live CDS file: one STRICT_DNS cluster per target port, every
// endpoint health-checked on the first port. Bouncer rewrites it in place.
func Clusters(svc config.Service, hostnames []string) string {
	hosts := append([]string(nil), hostnames...)
	sort.Strings(hosts)
	return render(svc, hosts, true)
}

// SeedClusters is the cluster file a freshly created proxy starts with: the
// replicas' service alias, no health checks. It sits in the proxy's definition
// (and so its config hash), so it holds nothing but what ports and the
// service name decide; health settings arrive with the first live write.
func SeedClusters(svc config.Service) string {
	return render(svc, []string{SeedHost(svc)}, false)
}

func render(svc config.Service, hosts []string, live bool) string {
	var resources []any
	for _, port := range targets(svc) {
		var endpoints []any
		for _, h := range hosts {
			ep := obj{"address": obj{"socket_address": obj{"address": h, "port_value": port}}}
			if live {
				ep["health_check_config"] = obj{"port_value": svc.Ports[0].Target}
			}
			endpoints = append(endpoints, obj{"endpoint": ep})
		}
		c := obj{
			"@type":             "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			"name":              clusterName(port),
			"type":              "STRICT_DNS",
			"dns_refresh_rate":  "1s",
			"dns_lookup_family": "V4_ONLY",
			"connect_timeout":   "1s",
			"load_assignment": obj{
				"cluster_name": clusterName(port),
				"endpoints":    []any{obj{"lb_endpoints": endpoints}},
			},
		}
		if live {
			// No healthcheck.uri: anything answering HTTP below 500 is up (404 on / is
			// fine). An explicit uri must answer 2xx. Int64Range end is exclusive.
			hcEnd := 500
			if svc.Spec.HealthStrict {
				hcEnd = 300
			}
			c["common_lb_config"] = obj{"ignore_new_hosts_until_first_hc": true}
			// no_traffic_interval: a cluster that has had no traffic yet is checked
			// at this rate instead of interval, and Envoy's default is 60s. A host
			// is first checked the moment it is listed, usually before the app
			// listens, so on a fresh proxy (first up, host reboot) a replica that
			// was ready in seconds stayed unhealthy for a minute.
			c["health_checks"] = []any{obj{
				"timeout": "1s", "interval": "1s", "no_traffic_interval": "1s",
				"unhealthy_threshold": 2, "healthy_threshold": 1,
				"http_health_check": obj{"path": svc.Spec.HealthPath,
					"expected_statuses": []any{obj{"start": 200, "end": hcEnd}}},
			}}
		}
		resources = append(resources, c)
	}
	return mustJSON(obj{"resources": resources})
}

// Hostnames is the endpoint hostnames of a cluster file (its first cluster;
// every cluster lists the same hosts).
func Hostnames(cds string) ([]string, error) {
	var doc struct {
		Resources []struct {
			LoadAssignment struct {
				Endpoints []struct {
					LbEndpoints []struct {
						Endpoint struct {
							Address struct {
								SocketAddress struct {
									Address string `json:"address"`
								} `json:"socket_address"`
							} `json:"address"`
						} `json:"endpoint"`
					} `json:"lb_endpoints"`
				} `json:"endpoints"`
			} `json:"load_assignment"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(cds), &doc); err != nil {
		return nil, err
	}
	if len(doc.Resources) == 0 || len(doc.Resources[0].LoadAssignment.Endpoints) == 0 {
		return nil, fmt.Errorf("cluster file has no endpoints section")
	}
	var out []string
	for _, e := range doc.Resources[0].LoadAssignment.Endpoints[0].LbEndpoints {
		out = append(out, e.Endpoint.Address.SocketAddress.Address)
	}
	return out, nil
}

// Entrypoint seeds the cluster file only when the container has none yet, so
// a restart keeps the last list Bouncer wrote.
func Entrypoint() []string {
	return []string{"bash", "-c", `mkdir -p ` + ClusterDir + ` && ` +
		`{ [ -f ` + ClusterFile + ` ] || printf '%s' "$BOUNCER_CDS" > ` + ClusterFile + `; } && ` +
		`exec envoy --config-yaml "$BOUNCER_BOOTSTRAP" --log-level warn`}
}
