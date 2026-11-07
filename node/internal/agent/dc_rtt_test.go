package agent

import (
	"testing"
	"time"
)

func TestMtddDCStatsTakesMainDCsOnly(t *testing.T) {
	samples := parsePrometheusSamples("mtdd_me_ping_rtt_us{dc=\"-2\"} 17821\n" +
		"mtdd_me_ping_rtt_us{dc=\"2\"} 25034\n" +
		"mtdd_me_ping_rtt_us{dc=\"1\"} 113840\n" +
		"mtdd_me_pool_ready{dc=\"2\"} 2\n")
	st := mtddDCStats(samples, time.Unix(1_800_000_000, 0))
	if st == nil || len(st.DCs) != 2 || st.DCs[0].DC != 1 || st.DCs[1].DC != 2 {
		t.Fatalf("main DCs only, sorted: %+v", st)
	}
	if st.DCs[1].RTTMs < 25.03 || st.DCs[1].RTTMs > 25.04 {
		t.Fatalf("µs → ms: %v", st.DCs[1].RTTMs)
	}
	if mtddDCStats(parsePrometheusSamples("mtdd_me_pool_ready{dc=\"2\"} 2\n"), time.Now()) != nil {
		t.Fatal("no RTT series — no snapshot")
	}
}

func TestTelemtAutoAPIAddr(t *testing.T) {
	var c TelemtConfig
	if got := telemtAutoAPIAddr(&c); got != "http://127.0.0.1:9091" {
		t.Fatalf("default: %q", got)
	}
	c.Server.API.Listen = "127.0.0.2:9200"
	if got := telemtAutoAPIAddr(&c); got != "http://127.0.0.2:9200" {
		t.Fatalf("listen: %q", got)
	}
	c.Server.API.AuthHeader = "Bearer x"
	if telemtAutoAPIAddr(&c) != "" {
		t.Fatal("auth header unknown to the agent — no auto API")
	}
	c.Server.API.AuthHeader = ""
	off := false
	c.Server.API.Enabled = &off
	if telemtAutoAPIAddr(&c) != "" {
		t.Fatal("disabled API")
	}
}
