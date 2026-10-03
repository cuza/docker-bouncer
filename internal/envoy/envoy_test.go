package envoy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cuza/docker-bouncer/internal/config"
)

func service() config.Service {
	return config.Service{Name: "api", AdminPort: 9901,
		Spec:  config.Spec{HealthPath: "/health"},
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
		`"name":"port-8080"`, `"name":"port-9090"`, // listeners
		`"cluster":"port-8080"`, `"timeout":"0s"`,
		`"validate_clusters":false`,
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
		`"ignore_new_hosts_until_first_hc":true`, `"path":"/health"`,
		`"unhealthy_threshold":2`, `"healthy_threshold":1`,
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
