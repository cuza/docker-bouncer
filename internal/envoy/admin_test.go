package envoy

import "testing"

const clusters = `port-8080::default_priority::max_connections::1024
port-8080::172.18.0.4:8080::hostname::proj-api-app-1
port-8080::172.18.0.4:8080::health_flags::healthy
port-8080::172.18.0.4:8080::rq_active::0
port-8080::172.18.0.5:8080::hostname::proj-api-app-2
port-8080::172.18.0.5:8080::health_flags::/failed_active_hc
port-9090::172.18.0.4:9090::hostname::proj-api-app-1
port-9090::172.18.0.4:9090::health_flags::healthy
port-9090::172.18.0.5:9090::hostname::proj-api-app-2
port-9090::172.18.0.5:9090::health_flags::healthy
`

func TestParseClustersHealthyOnlyIfHealthyEverywhere(t *testing.T) {
	got := ParseClusters(clusters)
	if !got["proj-api-app-1"] || got["proj-api-app-2"] {
		t.Fatalf("got %v", got)
	}
}

func TestParseCounter(t *testing.T) {
	body := "cluster_manager.cds.update_rejected: 0\ncluster_manager.cds.update_success: 7\n"
	if n, ok := ParseCounter(body, "cluster_manager.cds.update_success"); !ok || n != 7 {
		t.Fatalf("got %d %v", n, ok)
	}
	if _, ok := ParseCounter(body, "nope"); ok {
		t.Fatal("missing counter must report !ok")
	}
}

// 172.18.0.5 = 0x050012AC little-endian → "050012AC"; port 8080 = 1F90.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 020012AC:A1B2 050012AC:1F90 01 00000000:00000000 00:00000000 00000000   101        0 1 1 0000000000000000 100 0 0 10 0
   1: 020012AC:A1B3 050012AC:1F90 06 00000000:00000000 00:00000000 00000000   101        0 2 1 0000000000000000 100 0 0 10 0
   2: 020012AC:A1B4 040012AC:1F90 01 00000000:00000000 00:00000000 00000000   101        0 3 1 0000000000000000 100 0 0 10 0
   3: 0100007F:26AD 0100007F:A1B5 01 00000000:00000000 00:00000000 00000000   101        0 4 1 0000000000000000 100 0 0 10 0
`

func TestConnCountEstablishedToReplicaOnly(t *testing.T) {
	if n := ConnCount(procNetTCP, []string{"172.18.0.5"}, []int{8080}); n != 1 {
		t.Fatalf("want 1 established (TIME_WAIT ignored), got %d", n)
	}
	if n := ConnCount(procNetTCP, []string{"172.18.0.9"}, []int{8080}); n != 0 {
		t.Fatalf("got %d", n)
	}
}
