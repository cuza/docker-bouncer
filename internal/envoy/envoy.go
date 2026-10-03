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

// Clusters is the CDS file: one STRICT_DNS cluster per target port, every endpoint
// health-checked on the first port.
func Clusters(svc config.Service, hostnames []string) string {
	hosts := append([]string(nil), hostnames...)
	sort.Strings(hosts)
	hcPort := svc.Ports[0].Target
	var resources []any
	for _, port := range targets(svc) {
		var endpoints []any
		for _, h := range hosts {
			endpoints = append(endpoints, obj{"endpoint": obj{
				"address":             obj{"socket_address": obj{"address": h, "port_value": port}},
				"health_check_config": obj{"port_value": hcPort},
			}})
		}
		resources = append(resources, obj{
			"@type":             "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			"name":              clusterName(port),
			"type":              "STRICT_DNS",
			"dns_refresh_rate":  "1s",
			"dns_lookup_family": "V4_ONLY",
			"connect_timeout":   "1s",
			"common_lb_config":  obj{"ignore_new_hosts_until_first_hc": true},
			"health_checks": []any{obj{
				"timeout": "1s", "interval": "1s",
				"unhealthy_threshold": 2, "healthy_threshold": 1,
				"http_health_check": obj{"path": svc.Spec.HealthPath},
			}},
			"load_assignment": obj{
				"cluster_name": clusterName(port),
				"endpoints":    []any{obj{"lb_endpoints": endpoints}},
			},
		})
	}
	return mustJSON(obj{"resources": resources})
}

// Entrypoint seeds the cluster file only when the container has none yet, so
// a restart keeps the last list Bouncer wrote.
func Entrypoint() []string {
	return []string{"bash", "-c", `mkdir -p ` + ClusterDir + ` && ` +
		`{ [ -f ` + ClusterFile + ` ] || printf '%s' "$BOUNCER_CDS" > ` + ClusterFile + `; } && ` +
		`exec envoy --config-yaml "$BOUNCER_BOOTSTRAP" --log-level warn`}
}
