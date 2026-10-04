package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func svc(ext map[string]any, ports []types.ServicePortConfig, expose ...string) types.ServiceConfig {
	// Image, Ports and Expose are promoted fields; go1.26 forbids them in literals.
	s := types.ServiceConfig{Name: "api"}
	s.Image, s.Ports, s.Expose = "registry/app@sha256:1", ports, expose
	if ext != nil {
		s.Extensions = types.Extensions{"x-bouncer": ext}
	}
	return s
}

func TestNoExtensionIsNotAService(t *testing.T) {
	got, err := Parse(svc(nil, nil, "8080"))
	if got != nil || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDefaults(t *testing.T) {
	got, err := Parse(svc(map[string]any{}, nil, "8080"))
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{
		BounceMethod: MethodCrossover, MarginFactor: 0.95, OverprovisionFactor: 1.0,
		MinTaskUptime: 10 * time.Second, HealthTimeout: 300 * time.Second, HealthPath: "/",
		DrainMethod: DrainEnvoy, DrainDelay: 60 * time.Second, HistoryMax: 100, ProxyImage: DefaultProxyImage,
	}
	if got.Spec.BounceMethod != want.BounceMethod || got.Spec.MarginFactor != want.MarginFactor ||
		got.Spec.OverprovisionFactor != want.OverprovisionFactor || got.Spec.MinTaskUptime != want.MinTaskUptime ||
		got.Spec.HealthTimeout != want.HealthTimeout || got.Spec.HealthPath != want.HealthPath || got.Spec.HealthStrict ||
		got.Spec.DrainMethod != want.DrainMethod || got.Spec.DrainDelay != want.DrainDelay ||
		got.Spec.HistoryMax != want.HistoryMax || got.Spec.ProxyImage != want.ProxyImage {
		t.Fatalf("got %+v, want %+v", got.Spec, want)
	}
	if got.Name != "api" || len(got.Ports) != 1 || got.Ports[0].Target != 8080 || got.AdminPort != 9901 {
		t.Fatalf("got %+v", got)
	}
}

func TestOverrides(t *testing.T) {
	got, err := Parse(svc(map[string]any{
		"bounce_method": "upthendown", "bounce_margin_factor": 0.5, "bounce_overprovision_factor": 0.25,
		"min_task_uptime": "3s", "bounce_health_timeout": "2m",
		"healthcheck":         map[string]any{"mode": "http", "uri": "/ready"},
		"drain_method":        "noop",
		"drain_method_params": map[string]any{"delay": "95s"},
		"history_max":         5, "proxy_image": "envoyproxy/envoy:v1.39.0",
	}, nil, "8080"))
	if err != nil {
		t.Fatal(err)
	}
	s := got.Spec
	if s.BounceMethod != MethodUpThenDown || s.MarginFactor != 0.5 || s.OverprovisionFactor != 0.25 ||
		s.MinTaskUptime != 3*time.Second || s.HealthTimeout != 2*time.Minute || s.HealthPath != "/ready" || !s.HealthStrict ||
		s.DrainMethod != DrainNoop || s.DrainDelay != 95*time.Second || s.HistoryMax != 5 ||
		s.ProxyImage != "envoyproxy/envoy:v1.39.0" {
		t.Fatalf("got %+v", s)
	}
}

func TestPublishedPorts(t *testing.T) {
	got, err := Parse(svc(map[string]any{}, []types.ServicePortConfig{
		{Target: 8080, Published: "9000", HostIP: "127.0.0.1", Protocol: "tcp"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	p := got.Ports[0]
	if p.Target != 8080 || p.Published != "9000" || p.HostIP != "127.0.0.1" {
		t.Fatalf("got %+v", p)
	}
}

func TestAdminPortAvoidsServicePorts(t *testing.T) {
	got, err := Parse(svc(map[string]any{}, nil, "9901"))
	if err != nil {
		t.Fatal(err)
	}
	if got.AdminPort != 19901 {
		t.Fatalf("admin port %d", got.AdminPort)
	}
	_, err = Parse(svc(map[string]any{}, nil, "9901", "19901"))
	if err == nil || !strings.Contains(err.Error(), "admin port") {
		t.Fatalf("want admin port error, got %v", err)
	}
}

func TestValidationProblemsAreCollected(t *testing.T) {
	s := svc(map[string]any{"bounce_method": "sideways", "bounce_margin_factor": 1.5, "drain_method": "smoke"},
		[]types.ServicePortConfig{{Target: 53, Protocol: "udp"}, {Target: 7000, AppProtocol: "tcp"}})
	s.ContainerName = "fixed"
	_, err := Parse(s)
	var cerr *Error
	if err == nil {
		t.Fatal("want error")
	}
	cerr = err.(*Error)
	for _, want := range []string{"bounce_method", "bounce_margin_factor", "drain_method", "udp", "app_protocol", "container_name"} {
		if !strings.Contains(strings.Join(cerr.Problems, "\n"), want) {
			t.Errorf("missing problem about %q in %v", want, cerr.Problems)
		}
	}
}

func TestServiceWithoutPortsIsAnError(t *testing.T) {
	_, err := Parse(svc(map[string]any{}, nil))
	if err == nil || !strings.Contains(err.Error(), "ports") {
		t.Fatalf("got %v", err)
	}
}

func TestExposeAcceptsProtocolSuffixAndRejectsRanges(t *testing.T) {
	got, err := Parse(svc(map[string]any{}, nil, "8001/tcp"))
	if err != nil || got.Ports[0].Target != 8001 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := Parse(svc(map[string]any{}, nil, "8000-8002")); err == nil {
		t.Fatal("ranges must be rejected")
	}
}

func TestHTTPDrainNeedsIsSafeToKill(t *testing.T) {
	_, err := Parse(svc(map[string]any{"drain_method": "http",
		"drain_method_params": map[string]any{"drain": map[string]any{"path": "/drain"}}}, nil, "8080"))
	if err == nil || !strings.Contains(err.Error(), "is_safe_to_kill") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	_, err := Parse(svc(map[string]any{"drain_metod": "noop"}, nil, "8080"))
	if err == nil || !strings.Contains(err.Error(), "drain_metod") {
		t.Fatalf("got %v", err)
	}
}

func TestStaticAddressesAreRejected(t *testing.T) {
	s := svc(map[string]any{}, nil, "8080")
	s.MacAddress = "02:42:ac:11:00:02"
	s.Networks = map[string]*types.ServiceNetworkConfig{
		"front": {Ipv4Address: "172.20.0.5", Ipv6Address: "fd00::5", MacAddress: "02:42:ac:11:00:03"},
		"back":  nil,
	}
	_, err := Parse(s)
	if err == nil {
		t.Fatal("want error")
	}
	got := "\n" + strings.Join(err.(*Error).Problems, "\n")
	for _, want := range []string{"\nmac_address cannot", "front: ipv4_address", "front: ipv6_address", "front: mac_address"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing problem %q in %v", want, got)
		}
	}
}

func TestHistoryMaxBounds(t *testing.T) {
	for _, n := range []int{-1, 1001} {
		if _, err := Parse(svc(map[string]any{"history_max": n}, nil, "8080")); err == nil || !strings.Contains(err.Error(), "history_max") {
			t.Errorf("history_max %d: got %v", n, err)
		}
	}
}

// websocket is always allowed; upgrade_types adds more, lowercased and deduplicated.
func TestUpgradeTypes(t *testing.T) {
	got, err := Parse(svc(map[string]any{}, nil, "8080"))
	if err != nil || !slices.Equal(got.Spec.UpgradeTypes, []string{"websocket"}) {
		t.Fatalf("default %v %v", got, err)
	}
	got, err = Parse(svc(map[string]any{"upgrade_types": []any{"Tailscale-Control-Protocol", "WebSocket", "DERP"}}, nil, "8080"))
	if err != nil || !slices.Equal(got.Spec.UpgradeTypes, []string{"websocket", "tailscale-control-protocol", "derp"}) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := Parse(svc(map[string]any{"upgrade_types": []any{"", "a b"}}, nil, "8080")); err == nil || len(err.(*Error).Problems) != 2 {
		t.Fatalf("empty and spaced types must be rejected: %v", err)
	}
	bad := []any{"foo/bar", "foo:bar", `say"hi"`, "tab\there", "ctl\x01", "a(b)", "x=y"}
	if _, err := Parse(svc(map[string]any{"upgrade_types": bad}, nil, "8080")); err == nil || len(err.(*Error).Problems) != len(bad) {
		t.Fatalf("every non-token value must be rejected: %v", err)
	}
	if _, err := Parse(svc(map[string]any{"upgrade_types": []any{"h2c", "x-custom_1.0~", "a!#$%&'*+^`|"}}, nil, "8080")); err != nil {
		t.Fatalf("tchar values must pass: %v", err)
	}
}
