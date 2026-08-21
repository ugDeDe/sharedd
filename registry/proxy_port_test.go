package main

import (
	"strings"
	"testing"
	"time"
)

func TestProxyPortCompatibilityBlocksMasterEligibility(t *testing.T) {
	ok, bad := true, false
	c := &Candidate{Healthy: true, GlobalpingOK: true, MetricsHealthy: true, LastReportAt: time.Now(), Port: 8443, PortCompatible: &bad}
	if c.IsFullyHealthy(time.Minute) {
		t.Fatal("node on a port different from registry must not enter the healthy queue")
	}
	if reason := c.unhealthyReason(time.Minute); !strings.Contains(reason, "proxy port 8443") {
		t.Fatalf("unexpected reason: %q", reason)
	}
	c.PortCompatible = &ok
	if !c.IsFullyHealthy(time.Minute) {
		t.Fatal("matching port must permit otherwise healthy node")
	}
}
