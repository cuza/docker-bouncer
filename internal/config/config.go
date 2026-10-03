// Package config reads a service's x-bouncer block.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

const (
	Extension         = "x-bouncer"
	DefaultProxyImage = "envoyproxy/envoy:v1.39.1"

	MethodCrossover  = "crossover"
	MethodUpThenDown = "upthendown"
	MethodDownThenUp = "downthenup"
	MethodBrutal     = "brutal"

	DrainEnvoy = "envoy"
	DrainHTTP  = "http"
	DrainNoop  = "noop"
)

type HTTPCall struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	SuccessCodes []int  `json:"success_codes"`
}

type HTTPDrain struct {
	Drain, IsSafeToKill, StopDraining *HTTPCall
}

type Spec struct {
	BounceMethod        string
	MarginFactor        float64
	OverprovisionFactor float64
	MinTaskUptime       time.Duration
	HealthTimeout       time.Duration
	HealthPath          string
	HealthStrict        bool // healthcheck.uri set: only 2xx is healthy; else any status below 500
	DrainMethod         string
	DrainDelay          time.Duration
	DrainHTTP           HTTPDrain
	HistoryMax          int
	ProxyImage          string
}

type Port struct {
	Target    int
	Published string
	HostIP    string
}

type Service struct {
	Name      string
	Spec      Spec
	Ports     []Port
	AdminPort int
}

type Error struct {
	Service  string
	Problems []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("service %q: %s", e.Service, strings.Join(e.Problems, "; "))
}

// raw mirrors the YAML keys; pointers tell "unset" from zero.
type raw struct {
	BounceMethod        *string  `json:"bounce_method"`
	MarginFactor        *float64 `json:"bounce_margin_factor"`
	OverprovisionFactor *float64 `json:"bounce_overprovision_factor"`
	MinTaskUptime       *string  `json:"min_task_uptime"`
	HealthTimeout       *string  `json:"bounce_health_timeout"`
	Healthcheck         *struct {
		Mode string `json:"mode"`
		URI  string `json:"uri"`
	} `json:"healthcheck"`
	DrainMethod *string `json:"drain_method"`
	DrainParams *struct {
		Delay        *string   `json:"delay"`
		Drain        *HTTPCall `json:"drain"`
		IsSafeToKill *HTTPCall `json:"is_safe_to_kill"`
		StopDraining *HTTPCall `json:"stop_draining"`
	} `json:"drain_method_params"`
	HistoryMax *int    `json:"history_max"`
	ProxyImage *string `json:"proxy_image"`
}

// Parse returns nil, nil for a service without x-bouncer.
func Parse(svc types.ServiceConfig) (*Service, error) {
	ext, ok := svc.Extensions[Extension]
	if !ok {
		return nil, nil
	}
	e := &Error{Service: svc.Name}
	bad := func(f string, a ...any) { e.Problems = append(e.Problems, fmt.Sprintf(f, a...)) }

	var r raw
	if b, err := json.Marshal(ext); err != nil {
		bad("x-bouncer: %v", err)
	} else {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			bad("x-bouncer: %v", err)
		}
	}

	s := Spec{
		BounceMethod: MethodCrossover, MarginFactor: 0.95, OverprovisionFactor: 1.0,
		MinTaskUptime: 10 * time.Second, HealthTimeout: 300 * time.Second, HealthPath: "/",
		DrainMethod: DrainEnvoy, DrainDelay: 60 * time.Second, HistoryMax: 100, ProxyImage: DefaultProxyImage,
	}
	dur := func(name string, v *string, into *time.Duration) {
		if v == nil {
			return
		}
		d, err := time.ParseDuration(*v)
		if err != nil || d < 0 {
			bad("%s: %q is not a duration", name, *v)
			return
		}
		*into = d
	}
	if r.BounceMethod != nil {
		s.BounceMethod = *r.BounceMethod
	}
	switch s.BounceMethod {
	case MethodCrossover, MethodUpThenDown, MethodDownThenUp, MethodBrutal:
	default:
		bad("bounce_method: %q is not crossover, upthendown, downthenup or brutal", s.BounceMethod)
	}
	if r.MarginFactor != nil {
		s.MarginFactor = *r.MarginFactor
	}
	if s.MarginFactor < 0 || s.MarginFactor > 1 {
		bad("bounce_margin_factor: %v is outside 0..1", s.MarginFactor)
	}
	if r.OverprovisionFactor != nil {
		s.OverprovisionFactor = *r.OverprovisionFactor
	}
	if s.OverprovisionFactor < 0 || s.OverprovisionFactor > 1 {
		bad("bounce_overprovision_factor: %v is outside 0..1", s.OverprovisionFactor)
	}
	dur("min_task_uptime", r.MinTaskUptime, &s.MinTaskUptime)
	dur("bounce_health_timeout", r.HealthTimeout, &s.HealthTimeout)
	if hc := r.Healthcheck; hc != nil {
		if hc.Mode != "" && hc.Mode != "http" {
			bad("healthcheck.mode: only http is supported")
		}
		if hc.URI != "" {
			s.HealthPath, s.HealthStrict = hc.URI, true
		}
	}
	if r.DrainMethod != nil {
		s.DrainMethod = *r.DrainMethod
	}
	switch s.DrainMethod {
	case DrainEnvoy, DrainHTTP, DrainNoop:
	default:
		bad("drain_method: %q is not envoy, http or noop", s.DrainMethod)
	}
	if p := r.DrainParams; p != nil {
		dur("drain_method_params.delay", p.Delay, &s.DrainDelay)
		s.DrainHTTP = HTTPDrain{Drain: p.Drain, IsSafeToKill: p.IsSafeToKill, StopDraining: p.StopDraining}
	}
	if s.DrainMethod == DrainHTTP && (s.DrainHTTP.Drain == nil || s.DrainHTTP.IsSafeToKill == nil) {
		bad("drain_method http needs drain_method_params.drain and is_safe_to_kill")
	}
	if r.HistoryMax != nil {
		s.HistoryMax = *r.HistoryMax
	}
	if s.HistoryMax < 0 || s.HistoryMax > 1000 {
		bad("history_max must be 0..1000")
	}
	if r.ProxyImage != nil {
		s.ProxyImage = *r.ProxyImage
	}
	if svc.ContainerName != "" {
		bad("container_name cannot be set on a bouncer service (it runs several replicas)")
	}
	// The proxy and every replica would get the same address.
	if svc.MacAddress != "" {
		bad("mac_address cannot be set on a bouncer service (it runs several containers)")
	}
	for name, n := range svc.Networks {
		if n == nil {
			continue
		}
		for _, f := range [][2]string{{"ipv4_address", n.Ipv4Address}, {"ipv6_address", n.Ipv6Address}, {"mac_address", n.MacAddress}} {
			if f[1] != "" {
				bad("networks: %s: %s cannot be set on a bouncer service (it runs several containers)", name, f[0])
			}
		}
	}

	ports := parsePorts(svc, bad)
	if len(ports) == 0 {
		bad("a bouncer service needs ports: or expose:")
	}
	admin := 0
	for _, candidate := range []int{9901, 19901} {
		if !hasTarget(ports, candidate) {
			admin = candidate
			break
		}
	}
	if admin == 0 {
		bad("admin port: both 9901 and 19901 are service ports")
	}
	if len(e.Problems) > 0 {
		return nil, e
	}
	return &Service{Name: svc.Name, Spec: s, Ports: ports, AdminPort: admin}, nil
}

func parsePorts(svc types.ServiceConfig, bad func(string, ...any)) []Port {
	var out []Port
	add := func(p Port) {
		if !hasTarget(out, p.Target) || p.Published != "" {
			out = append(out, p)
		}
	}
	for _, p := range svc.Ports {
		if p.Protocol != "" && p.Protocol != "tcp" {
			bad("ports: %d/%s, only tcp is supported", p.Target, p.Protocol)
			continue
		}
		if p.AppProtocol != "" && p.AppProtocol != "http" {
			bad("ports: %d app_protocol %q, only http is supported in v1", p.Target, p.AppProtocol)
			continue
		}
		add(Port{Target: int(p.Target), Published: p.Published, HostIP: p.HostIP})
	}
	for _, e := range svc.Expose {
		n, err := strconv.Atoi(strings.TrimSuffix(e, "/tcp"))
		if err != nil || n <= 0 || n > 65535 {
			bad("expose: %q, only single tcp ports are supported", e)
			continue
		}
		add(Port{Target: n})
	}
	return out
}

func hasTarget(ports []Port, n int) bool {
	for _, p := range ports {
		if p.Target == n {
			return true
		}
	}
	return false
}
