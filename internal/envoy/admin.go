package envoy

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseClusters reads /clusters text: hostname → healthy in every cluster.
func ParseClusters(body string) map[string]bool {
	hostOf := map[string]string{} // "cluster::addr" → hostname
	flags := map[string][]string{}
	for _, line := range strings.Split(body, "\n") {
		f := strings.Split(line, "::")
		if len(f) != 4 {
			continue
		}
		key := f[0] + "::" + f[1]
		switch f[2] {
		case "hostname":
			hostOf[key] = f[3]
		case "health_flags":
			flags[key] = append(flags[key], f[3])
		}
	}
	out := map[string]bool{}
	for key, host := range hostOf {
		healthy := len(flags[key]) > 0
		for _, fl := range flags[key] {
			if fl != "healthy" {
				healthy = false
			}
		}
		if prev, seen := out[host]; seen {
			healthy = healthy && prev
		}
		out[host] = healthy
	}
	return out
}

// ParseCounter reads one "name: value" line from /stats text.
func ParseCounter(body, name string) (int64, bool) {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if ok && k == name {
			n, err := strconv.ParseInt(v, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// ConnCount counts ESTABLISHED connections to any ip:port in /proc/net/tcp.
func ConnCount(procNetTCP string, ips []string, ports []int) int {
	want := map[string]bool{}
	for _, ip := range ips {
		var a, b, c, d int
		if _, err := fmt.Sscanf(ip, "%d.%d.%d.%d", &a, &b, &c, &d); err != nil {
			continue
		}
		for _, p := range ports {
			want[fmt.Sprintf("%02X%02X%02X%02X:%04X", d, c, b, a, p)] = true
		}
	}
	n := 0
	for _, line := range strings.Split(procNetTCP, "\n") {
		f := strings.Fields(line)
		if len(f) > 3 && f[3] == "01" && want[f[2]] {
			n++
		}
	}
	return n
}
