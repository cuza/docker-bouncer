package envoy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cuza/docker-bouncer/internal/config"
)

func service() config.Service {
	return config.Service{Name: "api", AdminPort: 9901,
		Spec:  config.Spec{HealthPath: "/", UpgradeTypes: []string{"websocket", "derp"}},
		Ports: []config.Port{{Target: 8080, Published: "8080", HostIP: "127.0.0.1"}, {Target: 9090}}}
}

func TestBootstrapListenersRoutesAndAdmin(t *testing.T) {
	var b map[string]any
	if err := json.Unmarshal([]byte(Bootstrap(service())), &b); err != nil {
		t.Fatal(err)
	}
	s := Bootstrap(service())
	for _, want := range []string{
		`"port_value":9901`, `"address":"127.0.0.1"`, // admin
		`{"address":"::","ipv4_compat":true,"port_value":8080}`, // dual-stack listener
		`"name":"port-8080"`, `"name":"port-9090"`, // listeners
		`"cluster":"port-8080"`, `"timeout":"0s"`,
		`"validate_clusters":false`,
		`"use_remote_address":true`, `"xff_num_trusted_hops":1`, `"preserve_external_request_id":true`,
		`"upgrade_configs":[{"upgrade_type":"websocket"},{"upgrade_type":"derp"}]`,
		`"stream_idle_timeout":"0s"`,
		`"retry_on":"connect-failure,refused-stream,reset-before-request"`,
		`"num_retries":3`, `envoy.retry_host_predicates.previous_hosts`,
		`"path":"/etc/bouncer/dyn/cds.json"`, `"watched_directory":{"path":"/etc/bouncer/dyn"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap lacks %s", want)
		}
	}
}

func TestBootstrapIsDeterministicAndHasNoReplicas(t *testing.T) {
	a, b := Bootstrap(service()), Bootstrap(service())
	if a != b {
		t.Fatal("bootstrap differs between calls")
	}
	if strings.Contains(a, "api-app") {
		t.Fatal("bootstrap must not contain replica names (it is part of the proxy's config hash)")
	}
}

func TestClustersOnePerPortWithHostnamesAndHealthCheck(t *testing.T) {
	s := Clusters(service(), []string{"proj-api-app-2", "proj-api-app-1"})
	var doc struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Resources) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(doc.Resources))
	}
	for _, want := range []string{
		`"type":"STRICT_DNS"`, `"dns_refresh_rate":"1s"`, `"dns_lookup_family":"V4_ONLY"`,
		`"ignore_new_hosts_until_first_hc":true`,
		`"http_health_check":{"expected_statuses":[{"end":500,"start":200}],"path":"/"}`, // default: any non-5xx
		`"unhealthy_threshold":2`, `"healthy_threshold":1`,
		`"interval":"1s"`, `"no_traffic_interval":"1s"`, // a proxy with no traffic yet must not wait Envoy's 60s
		`"port_value":9090`,                         // the 9090 cluster's endpoints
		`"health_check_config":{"port_value":8080}`, // every cluster checks the first port
	} {
		if !strings.Contains(s, want) {
			t.Errorf("clusters lack %s", want)
		}
	}
	if strings.Index(s, "proj-api-app-1") > strings.Index(s, "proj-api-app-2") {
		t.Error("hostnames must be sorted")
	}
}

func TestClustersExplicitHealthURIRequires2xx(t *testing.T) {
	svc := service()
	svc.Spec.HealthPath, svc.Spec.HealthStrict = "/health", true
	if s := Clusters(svc, []string{"h"}); !strings.Contains(s, `"http_health_check":{"expected_statuses":[{"end":300,"start":200}],"path":"/health"}`) {
		t.Fatalf("clusters: %s", s)
	}
}

func TestSeedHasAliasAndNoHealthSettings(t *testing.T) {
	s := SeedClusters(service())
	for _, want := range []string{`"address":"api-app"`, `"type":"STRICT_DNS"`, `"connect_timeout":"1s"`, `"name":"port-9090"`} {
		if !strings.Contains(s, want) {
			t.Errorf("seed lacks %s", want)
		}
	}
	for _, bad := range []string{"health", "ignore_new_hosts"} {
		if strings.Contains(s, bad) {
			t.Errorf("seed must not carry %s settings: %s", bad, s)
		}
	}
	strict := service()
	strict.Spec.HealthPath, strict.Spec.HealthStrict = "/health", true
	if SeedClusters(strict) != s {
		t.Error("seed must not depend on health config")
	}
}

func TestHostnamesReadsClusterFile(t *testing.T) {
	h, err := Hostnames(Clusters(service(), []string{"b", "a"}))
	if err != nil || strings.Join(h, ",") != "a,b" {
		t.Fatalf("%v %v", h, err)
	}
	if h, err := Hostnames(Clusters(service(), nil)); err != nil || len(h) != 0 {
		t.Fatalf("empty list: %v %v", h, err)
	}
	if _, err := Hostnames("garbage"); err == nil {
		t.Fatal("garbage must not parse")
	}
}

func TestEntrypointSeedsOnlyWhenAbsent(t *testing.T) {
	e := strings.Join(Entrypoint(), " ")
	if !strings.Contains(e, `[ -f /etc/bouncer/dyn/cds.json ] ||`) || !strings.Contains(e, `exec envoy --config-yaml "$BOUNCER_BOOTSTRAP"`) {
		t.Fatalf("entrypoint: %s", e)
	}
}

func TestOneListenerAndClusterPerTarget(t *testing.T) {
	svc := service()
	svc.Ports = []config.Port{{Target: 8080, Published: "8080"}, {Target: 8080, Published: "8081"}}
	if n := strings.Count(Bootstrap(svc), `"name":"port-8080"`); n != 1 {
		t.Errorf("want 1 listener for target 8080, got %d", n)
	}
	var doc struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(Clusters(svc, []string{"a"})), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Resources) != 1 {
		t.Errorf("want 1 cluster for target 8080, got %d", len(doc.Resources))
	}
}
